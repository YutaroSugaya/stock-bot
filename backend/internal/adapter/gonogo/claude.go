package gonogo

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"stockbot/backend/internal/adapter/advisor"
	"stockbot/backend/internal/port"
)

// PromptVersion はプロンプトの版。記録に残し、版をまたいだ判定は採点で混ぜない。
// prompt.md を変えたら上げること。
const PromptVersion = "gonogo-v1"

// プロンプトはバイナリに埋め込む(launchd 配下のバイナリは repo に触れた瞬間に止められる)。
//
//go:embed prompt.md
var promptText string

// 🛑 許すツールは WebSearch と WebFetch だけ。`--tools` で使える集合を絞り、権限層の
// `--disallowed-tools` でもう一度塞ぐ(advisor と同じ二重化)。
const (
	allowedTools    = "WebSearch,WebFetch"
	disallowedTools = "Bash,Edit,Write,NotebookEdit,Read"
)

// 🛑 モデルはエイリアスで書く(版付き ID は古い世代に据え置かれる)。effort は全銘柄が寄り前に
// 終わるように選ぶ(advisor の max は 1 銘柄 10 分かかった)。
var judgeModelArgs = []string{"--model", "opus", "--effort", "medium"}

func judgeArgs() []string {
	args := []string{"-p", "--no-session-persistence", "--tools", allowedTools,
		"--allowed-tools", allowedTools, "--disallowed-tools", disallowedTools, "--output-format", "json"}
	return append(args, judgeModelArgs...)
}

// ErrUsageLimit は claude の利用上限。その実行の残りは判定せずに終える(次の起動経路が拾う)。
var ErrUsageLimit = errors.New("usage_limit")

// Judge は 1 銘柄の判定を claude CLI で出す。
type Judge struct {
	CLIPath string
	WorkDir string // 子プロセスの CWD(~/.stockbot/tmp。repo に触れない)
	Timeout time.Duration
	// ExecCommand はテスト差し替え用(exec.CommandContext と同じ形)。
	ExecCommand advisor.ExecCommandFunc
	// 一過性のエラー(529・接続断)は 1 回だけ再試行する。利用上限は再試行しない。
	TransientRetries int
	TransientBackoff time.Duration
}

func NewJudge(cli, workDir string, timeout time.Duration) *Judge {
	return &Judge{CLIPath: cli, WorkDir: workDir, Timeout: timeout, ExecCommand: exec.CommandContext,
		TransientRetries: 1, TransientBackoff: 10 * time.Second}
}

// Judge は packet(決定論の材料の JSON)を渡して判定を返す。model は答えたモデル(envelope から)。
func (j *Judge) Judge(ctx context.Context, packet []byte) (port.GoNoGoJudgment, string, error) {
	if j.CLIPath == "" {
		return port.GoNoGoJudgment{}, "", errors.New("claude cli path is empty")
	}
	if j.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, j.Timeout)
		defer cancel()
	}
	stdin := promptText + "\n```json\n" + string(packet) + "\n```\n"
	var lastErr error
	for attempt := 0; attempt <= j.TransientRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return port.GoNoGoJudgment{}, "", fmt.Errorf("timeout: %w", ctx.Err())
			case <-time.After(j.TransientBackoff):
			}
		}
		cmd := j.ExecCommand(ctx, j.CLIPath, judgeArgs()...)
		if j.WorkDir != "" {
			cmd.Dir = j.WorkDir
		}
		cmd.Stdin = strings.NewReader(stdin)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		runErr := cmd.Run()
		if ctx.Err() != nil {
			return port.GoNoGoJudgment{}, "", fmt.Errorf("timeout: %w", ctx.Err())
		}
		body, model, ok := advisor.CLIEnvelope(stdout.Bytes())
		if !ok {
			body = stdout.String()
		}
		// exit 0 で読める判定が出ていれば、それが答え(要約の文面を障害の文面と取り違えない)。
		var parseErr error
		if runErr == nil {
			jd, err := ParseJudgment(body)
			if err == nil {
				return jd, model, nil
			}
			parseErr = err
		}
		switch {
		case advisor.IsTransientOutput(body) || advisor.IsTransientOutput(stderr.String()):
			lastErr = fmt.Errorf("transient: %s", firstLine(body+" "+stderr.String()))
			continue
		case advisor.IsUsageLimitOutput(body) || advisor.IsUsageLimitOutput(stderr.String()):
			return port.GoNoGoJudgment{}, model, fmt.Errorf("%w: %s", ErrUsageLimit, firstLine(body+" "+stderr.String()))
		case runErr != nil:
			return port.GoNoGoJudgment{}, model, fmt.Errorf("claude: %v: %s", runErr, firstLine(stderr.String()+" "+body))
		}
		return port.GoNoGoJudgment{}, model, parseErr
	}
	return port.GoNoGoJudgment{}, "", lastErr
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}
