package gonogo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/advisor"
	"stockbot/backend/internal/port"
)

// fakeCLI は claude の代わりにテストバイナリを起動し、決めた stdout と終了コードを返させる。
// calls は呼ばれた回数、seen は最後の引数。
func fakeCLI(outs []string, exits []int, calls *int32, seen *[]string) advisor.ExecCommandFunc {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		i := int(atomic.AddInt32(calls, 1)) - 1
		if i >= len(outs) {
			i = len(outs) - 1
		}
		*seen = args
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGoNoGoHelperProcess", "--", outs[i], fmt.Sprint(exits[i]))
		cmd.Env = append(os.Environ(), "GO_WANT_GONOGO_HELPER=1")
		return cmd
	}
}

func TestGoNoGoHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_GONOGO_HELPER") != "1" {
		return
	}
	for i, a := range os.Args {
		if a == "--" {
			fmt.Fprint(os.Stdout, os.Args[i+1])
			if os.Args[i+2] == "0" {
				os.Exit(0)
			}
			os.Exit(1)
		}
	}
	os.Exit(0)
}

func envelope(result string) string {
	b := strings.ReplaceAll(result, `"`, `\"`)
	return `{"type":"result","result":"` + b + `","modelUsage":{"claude-opus-x":{}}}`
}

const okJSON = `{"verdict":"go","category":"none","confidence":"mid","summary_ja":"地合いの連れ安","sources":[]}`

func newJudge(outs []string, exits []int, calls *int32, seen *[]string) *Judge {
	j := NewJudge("claude", "", time.Minute)
	j.ExecCommand = fakeCLI(outs, exits, calls, seen)
	j.TransientBackoff = time.Millisecond
	return j
}

func TestJudge_Success(t *testing.T) {
	var calls int32
	var seen []string
	j, model, err := newJudge([]string{envelope(okJSON)}, []int{0}, &calls, &seen).Judge(context.Background(), []byte(`{}`))
	if err != nil || j.Verdict != port.GoNoGoGo || model != "claude-opus-x" {
		t.Fatalf("got %+v %q %v", j, model, err)
	}
}

// 許すツールは WebSearch と WebFetch だけ。Bash / Edit / Write / NotebookEdit / Read は塞ぐ。版は書かない。
func TestJudge_ToolsAndModelArgs(t *testing.T) {
	var calls int32
	var seen []string
	_, _, _ = newJudge([]string{envelope(okJSON)}, []int{0}, &calls, &seen).Judge(context.Background(), []byte(`{}`))
	args := strings.Join(seen, " ")
	for _, want := range []string{"-p", "--no-session-persistence", "--output-format json", "--tools WebSearch,WebFetch",
		"--allowed-tools WebSearch,WebFetch", "--model opus"} {
		if !strings.Contains(args, want) {
			t.Errorf("%q が無い: %s", want, args)
		}
	}
	i := strings.Index(args, "--disallowed-tools ")
	if i < 0 {
		t.Fatalf("--disallowed-tools が無い: %s", args)
	}
	deny := strings.Fields(args[i+len("--disallowed-tools "):])[0]
	for _, tool := range []string{"Bash", "Edit", "Write", "NotebookEdit", "Read"} {
		if !strings.Contains(deny, tool) {
			t.Errorf("%s を塞いでいない: %s", tool, deny)
		}
	}
	if strings.Contains(args, "claude-opus") || strings.Contains(args, "claude-sonnet") {
		t.Errorf("モデルの版を書いている: %s", args)
	}
}

func TestJudge_UsageLimit(t *testing.T) {
	var calls int32
	var seen []string
	_, _, err := newJudge([]string{"Claude AI usage limit reached|1760000000"}, []int{1}, &calls, &seen).
		Judge(context.Background(), []byte(`{}`))
	if !errors.Is(err, ErrUsageLimit) {
		t.Fatalf("利用上限を分類していない: %v", err)
	}
	if calls != 1 {
		t.Fatalf("利用上限で再試行した: %d 回", calls)
	}
}

// 一過性のエラーは 1 回だけ再試行する。
func TestJudge_RetriesTransientOnce(t *testing.T) {
	var calls int32
	var seen []string
	out := "API Error: 529 Overloaded"
	_, _, err := newJudge([]string{out, envelope(okJSON)}, []int{1, 0}, &calls, &seen).Judge(context.Background(), []byte(`{}`))
	if err != nil || calls != 2 {
		t.Fatalf("1 回の再試行で成功するはず: calls=%d err=%v", calls, err)
	}
	calls = 0
	_, _, err = newJudge([]string{out, out, out}, []int{1, 1, 1}, &calls, &seen).Judge(context.Background(), []byte(`{}`))
	if err == nil || calls != 2 {
		t.Fatalf("再試行は 1 回だけ: calls=%d err=%v", calls, err)
	}
}

func TestJudge_SchemaViolationIsError(t *testing.T) {
	var calls int32
	var seen []string
	_, _, err := newJudge([]string{envelope("わかりません")}, []int{0}, &calls, &seen).Judge(context.Background(), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("スキーマ違反: %v", err)
	}
}

func TestPromptIsEmbeddedAndVersioned(t *testing.T) {
	if !strings.Contains(promptText, "Web 検索") || PromptVersion == "" {
		t.Fatal("プロンプトが埋め込まれていない / 版が無い")
	}
}
