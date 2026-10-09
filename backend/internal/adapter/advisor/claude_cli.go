package advisor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// テスト差し替え用に exec.CommandContext と同じ形。
type ExecCommandFunc func(ctx context.Context, name string, args ...string) *exec.Cmd

// 🛑 版番号を書かない。`opus` は「最新の Opus」を指す claude CLI のエイリアスで、世代交代にコード変更なしで
// 追従する(版付き ID は古い世代に据え置かれ続けるので TestClaudeBaseArgs_NoVersionPin が機械的に拒否)。
// フラグごと外さないのは、CLI 既定が Opus 以外へ振られうるため。
// ⚠ エイリアスの解決表は **CLI のバージョン**に依存する(実測: v2.1.202 は opus → 4.8、
// v2.1.220 は opus → claude-opus-5)。最新世代に乗らないときはまず `claude --version` を疑う。
var claudeModelArgs = []string{"--model", "opus", "--effort", "max"}

// `--tools ""` を無視する CLI 版があっても素通りしないための二重化。
const disallowedTools = "Bash,Edit,Write,NotebookEdit,WebFetch,WebSearch"

// advisor は stdin を読んで YAML を吐くだけなのでツールは一切要らない(`--tools ""` + 権限層の
// `--disallowed-tools` の二重化で、子プロセスはマシン上で何もできない)。
// `--output-format json` は **どの世代のモデルが答えたかを知るためだけ** — 素のテキスト出力はモデル名を
// 報告しないので、無いと config の provenance が日付突き合わせでしか辿れない。
func claudeBaseArgs() []string {
	args := []string{"-p", "--no-session-persistence", "--tools", "", "--disallowed-tools", disallowedTools,
		"--output-format", "json"}
	return append(args, claudeModelArgs...)
}

type ClaudeCLIAdvisor struct {
	CLIPath        string
	PromptPath     string
	WorkingDir     string // 子プロセスの CWD。prompt が Read("prompts/skills/...") できるように
	OutputDir      string // 空なら保存しない。成功 → .yaml / 失敗 → .fail.yaml
	TimeoutSeconds int    // 既定 120
	Logger         *slog.Logger
	Clock          func() time.Time
	IDGen          func() string
	ExecCommand    ExecCommandFunc

	// 529/接続断など一過性のみ再試行。usage/session 上限は復帰に数時間かかるので再試行しない。
	TransientRetries      int
	TransientRetryBackoff time.Duration
}

func New(cli, promptPath, workingDir string, timeoutSec int) *ClaudeCLIAdvisor {
	return &ClaudeCLIAdvisor{
		CLIPath:               cli,
		PromptPath:            promptPath,
		WorkingDir:            workingDir,
		TimeoutSeconds:        timeoutSec,
		Logger:                slog.Default(),
		Clock:                 time.Now,
		IDGen:                 defaultRunID,
		ExecCommand:           exec.CommandContext,
		TransientRetries:      1,
		TransientRetryBackoff: 5 * time.Second,
	}
}

func defaultRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// 全ての失敗モードが fail-close(success 以外は呼び手が promotion を飛ばす)。
func (a *ClaudeCLIAdvisor) Generate(ctx context.Context, summary *market.MarketSummary) (*port.AdvisorRun, error) {
	if a.CLIPath == "" {
		return nil, errors.New("advisor: cli path empty")
	}
	payload, summaryJSON, err := (&PromptAssembler{PromptPath: a.PromptPath}).Build(summary)
	if err != nil {
		return nil, err
	}

	cctx, cancel := a.applyTimeout(ctx)
	defer cancel()

	run := &port.AdvisorRun{RunID: a.IDGen(), StartedAt: a.now(), InputJSON: summaryJSON}
	parser := ResponseParser{}

	attempts := a.TransientRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-cctx.Done():
				return a.finish(run, port.AdvisorRunTimeout, "claude CLI timed out during retry backoff"), nil
			case <-time.After(a.TransientRetryBackoff):
			}
		}

		cmd := a.ExecCommand(cctx, a.CLIPath, claudeBaseArgs()...)
		if a.WorkingDir != "" {
			cmd.Dir = a.WorkingDir
		}
		cmd.Stdin = bytes.NewReader(payload)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		runErr := cmd.Run()
		run.FinishedAt = a.now()

		// OutputYAML は「モデルが書いた本文」のまま保つ(envelope 全体を入れると監査ログの意味が途中で変わる)。
		body, model, ok := splitEnvelope(stdout.Bytes())
		if ok {
			run.Model = model
			run.OutputYAML = []byte(body)
		} else {
			run.OutputYAML = stdout.Bytes()
		}
		out := string(run.OutputYAML)

		if cctx.Err() == context.DeadlineExceeded {
			return a.finish(run, port.AdvisorRunTimeout, "claude CLI timed out"), nil
		}

		if runErr != nil {
			// claude は非ゼロ終了でもメッセージを stdout に出すので、exit 0 と同じ分類を先に通す。
			switch classifyCLIOutput(out) {
			case cliOutcomeUsageLimit:
				run.UsageLimited = true
				return a.finish(run, port.AdvisorRunCLIError, "claude usage/session limit: "+firstLine(out)), nil
			case cliOutcomeTransient:
				if attempt < attempts-1 {
					a.logRetry(run, "transient API error, retrying: "+firstLine(out))
					continue
				}
				return a.finish(run, port.AdvisorRunCLIError, "claude transient API error: "+firstLine(out)), nil
			default:
				var exitErr *exec.ExitError
				if errors.As(runErr, &exitErr) {
					return a.finish(run, port.AdvisorRunCLIError,
						fmt.Sprintf("claude exit %d: %s", exitErr.ExitCode(), truncate(stderr.String(), 500))), nil
				}
				return a.finish(run, port.AdvisorRunCLIError, runErr.Error()), nil
			}
		}

		// exit 0 でも infra/usage のメッセージが出ることがあるので、parse 失敗時に分類し直す。
		if parsed, perr := parser.Parse(run.OutputYAML); perr == nil {
			run.ParsedYAML = parsed
			return a.finish(run, port.AdvisorRunSuccess, ""), nil
		} else {
			switch {
			case classifyCLIOutput(out) == cliOutcomeUsageLimit:
				run.UsageLimited = true
				return a.finish(run, port.AdvisorRunCLIError, "claude usage/session limit: "+firstLine(out)), nil
			case classifyCLIOutput(out) == cliOutcomeTransient:
				if attempt < attempts-1 {
					a.logRetry(run, "transient API error, retrying: "+firstLine(out))
					continue
				}
				return a.finish(run, port.AdvisorRunCLIError, "claude transient API error: "+firstLine(out)), nil
			case errors.Is(perr, errEmptyStdout):
				return a.finish(run, port.AdvisorRunEmpty, "empty stdout"), nil
			default:
				return a.finish(run, port.AdvisorRunParseError, perr.Error()), nil
			}
		}
	}
	return run, nil
}

func (a *ClaudeCLIAdvisor) now() time.Time {
	if a.Clock != nil {
		return a.Clock()
	}
	return time.Now()
}

// ctx.Deadline と TimeoutSeconds の短い方を採る。TimeoutSeconds < 0 は制限なし(自前の deadline を張らない)
// — 打ち切ると毎 run timeout → fail-close で config を一切 arm せず advisor 経路の収集が丸ごと消えるので、
// 「途中で殺すくらいなら待つ」が研究モードの正しい倒し方。親 ctx の cancel は常に尊重する(SIGTERM で道連れ)。
// 0 は「yaml 未設定」と区別できないので既定 120 秒のまま。
func (a *ClaudeCLIAdvisor) applyTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	cfg := a.TimeoutSeconds
	if cfg < 0 {
		return context.WithCancel(ctx)
	}
	if cfg == 0 {
		cfg = 120
	}
	cfgDeadline := a.now().Add(time.Duration(cfg) * time.Second)
	if parent, ok := ctx.Deadline(); !ok || cfgDeadline.Before(parent) {
		return context.WithDeadline(ctx, cfgDeadline)
	}
	return context.WithCancel(ctx)
}

func (a *ClaudeCLIAdvisor) finish(r *port.AdvisorRun, status port.AdvisorRunStatus, msg string) *port.AdvisorRun {
	r.Status = status
	r.ErrorMsg = msg // 成功時も必ず上書き(前の再試行エラーを残さない)
	if r.FinishedAt.IsZero() {
		r.FinishedAt = a.now()
	}
	if a.Logger != nil {
		a.Logger.Info("advisor_run", "run_id", r.RunID, "status", string(status),
			"usage_limited", r.UsageLimited,
			"duration_ms", r.FinishedAt.Sub(r.StartedAt).Milliseconds(), "err", truncate(r.ErrorMsg, 200))
	}
	a.archive(r)
	return r
}

// 終端ステータスは押さない(後続 attempt の finish() と二重に終端ログが出る)。
func (a *ClaudeCLIAdvisor) logRetry(r *port.AdvisorRun, msg string) {
	if a.Logger != nil {
		a.Logger.Warn("advisor_run_retry", "run_id", r.RunID, "msg", truncate(msg, 200))
	}
}

// 入力(.input.json)も残す — 無いと「なぜその判断か」を再現できない。best-effort。
func (a *ClaudeCLIAdvisor) archive(r *port.AdvisorRun) {
	if a.OutputDir == "" {
		return
	}
	if err := os.MkdirAll(a.OutputDir, 0o755); err != nil {
		return
	}
	base := fmt.Sprintf("advisor_%s_%s", r.StartedAt.UTC().Format("20060102_150405"), r.RunID)
	if len(r.InputJSON) > 0 {
		_ = os.WriteFile(filepath.Join(a.OutputDir, base+".input.json"), r.InputJSON, 0o644)
	}
	if len(r.OutputYAML) == 0 {
		return
	}
	ext := ".yaml"
	if !r.Status.Promotable() {
		ext = ".fail.yaml"
	}
	_ = os.WriteFile(filepath.Join(a.OutputDir, base+ext), r.OutputYAML, 0o644)
}

var _ port.Advisor = (*ClaudeCLIAdvisor)(nil)
