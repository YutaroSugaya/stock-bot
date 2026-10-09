package advisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// fakeExec re-invokes the test binary in TestHelperProcess mode so we can script
// claude's stdout / exit code without a real `claude` binary.
func fakeExec(stdout string, exit int) ExecCommandFunc {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestHelperProcess", "--", stdout, fmt.Sprint(exit)}
		cmd := exec.CommandContext(ctx, os.Args[0], cs...)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	// args after "--": stdout, exit
	args := os.Args
	for i, a := range args {
		if a == "--" {
			out, code := args[i+1], args[i+2]
			fmt.Fprint(os.Stdout, out)
			if code == "0" {
				os.Exit(0)
			}
			os.Exit(1)
		}
	}
	os.Exit(0)
}

func newTestAdvisor(t *testing.T, stdout string, exit int) *ClaudeCLIAdvisor {
	t.Helper()
	// a throwaway prompt file
	pf := t.TempDir() + "/prompt.md"
	if err := os.WriteFile(pf, []byte("generate a config"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := New("claude", pf, "", 30)
	a.ExecCommand = fakeExec(stdout, exit)
	a.TransientRetries = 0
	a.IDGen = func() string { return "run-test" }
	return a
}

func summary() *market.MarketSummary { return &market.MarketSummary{Symbol: "7203"} }

// TestClaudeBaseArgs_NoVersionPin guards how the advisor selects its model:
// tier(opus)は固定・世代は追従。`--model` にはバージョン番号を含まない
// エイリアス(opus / sonnet / fable)だけを許し、`claude-opus-4-8` のような
// 版付き ID を拒否する。版付きで書くと世代交代のたびに旧世代へ据え置かれる。
// 特定世代に固定したくなったら、このテストを消す前に人間が理由を commit すること。
func TestClaudeBaseArgs_NoVersionPin(t *testing.T) {
	args := claudeBaseArgs()

	i := slices.Index(args, "--model")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("--model <alias> が無い(tier を CLI 既定任せにすると Opus 以外へ振られうる): args=%v", args)
	}
	model := args[i+1]

	// バージョン番号(数字)を含む = 版付き pin。エイリアスは英字のみ。
	if strings.ContainsAny(model, "0123456789") {
		t.Fatalf("--model に版付き ID を pin しない(エイリアスで世代追従が意図): %q", model)
	}
	if model != "opus" {
		t.Fatalf("advisor は opus tier で走らせる(config 生成は品質優先): got %q", model)
	}

	// effort は世代に依存しない品質ノブなので固定のまま。
	if !slices.Contains(args, "--effort") || !slices.Contains(args, "max") {
		t.Fatalf("--effort max が落ちている: args=%v", args)
	}
}

// configYAML は成功時に claude が返す最小の strategy_config。
const configYAML = "config_id: x\nstrategy_name: bnf_reversion\nmode: paper_config\nentry:\n  direction: buy_only\n"

// jsonEnvelope は `claude -p --output-format json` の応答形。実際の CLI 出力
// と同じ形にしてある。
func jsonEnvelope(t *testing.T, result, model string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"result":     result,
		"modelUsage": map[string]any{model: map[string]any{"inputTokens": 2, "outputTokens": 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestGenerate_CapturesModel — どの世代の LLM が config を書いたかを run に残す。
// advisor_runs に残さないと forward 記録の provenance が日付突き合わせでしか
// 辿れなくなる。
func TestGenerate_CapturesModel(t *testing.T) {
	a := newTestAdvisor(t, jsonEnvelope(t, configYAML, "claude-opus-5"), 0)
	run, err := a.Generate(context.Background(), summary())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Status != port.AdvisorRunSuccess {
		t.Fatalf("want success, got %s (%s)", run.Status, run.ErrorMsg)
	}
	if run.Model != "claude-opus-5" {
		t.Fatalf("model が記録されていない: got %q", run.Model)
	}
	// envelope の result 部分が config として解釈されること(JSON がそのまま
	// YAML パーサに渡ると parse_error になる)。
	if !strings.Contains(string(run.ParsedYAML), "strategy_name: bnf_reversion") {
		t.Fatalf("envelope の result が config として解釈されていない: %q", run.ParsedYAML)
	}
}

// TestGenerate_PlainStdoutStillParses — envelope でない素の YAML でも従来どおり
// 成功する(CLI が古い / --output-format を無視する場合の fail-safe)。
// その場合 Model は空(不明)で、記録の欠落として区別できる。
func TestGenerate_PlainStdoutStillParses(t *testing.T) {
	a := newTestAdvisor(t, configYAML, 0)
	run, err := a.Generate(context.Background(), summary())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Status != port.AdvisorRunSuccess {
		t.Fatalf("want success, got %s (%s)", run.Status, run.ErrorMsg)
	}
	if run.Model != "" {
		t.Fatalf("envelope でないのに model が入っている: %q", run.Model)
	}
}

// TestApplyTimeout — timeout_seconds の3状態。負値 = 制限なし(deadline を張らない)。
// Opus 5(effort max)は 4.8 の桁を超えて考えるので、打ち切ると毎 run timeout になり
// fail-closed で config を一切 arm しない = advisor 経路の収集がゼロになる。
// 「途中で殺すくらいなら待つ」が研究モードの正しい倒し方。
// 0 は yaml 未設定と区別できないので既定値のまま(制限なしにしない)。
func TestApplyTimeout(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sec         int
		wantDeadlin bool
	}{
		{"未設定は既定120秒", 0, true},
		{"明示指定は効く", 30, true},
		{"負値は制限なし", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &ClaudeCLIAdvisor{TimeoutSeconds: tc.sec}
			ctx, cancel := a.applyTimeout(context.Background())
			defer cancel()
			if _, ok := ctx.Deadline(); ok != tc.wantDeadlin {
				t.Fatalf("deadline=%v, want %v (sec=%d)", ok, tc.wantDeadlin, tc.sec)
			}
		})
	}
}

// 親 ctx の deadline は負値でも尊重する(bot 停止で advisor も道連れに止まること)。
func TestApplyTimeout_NoLimitStillHonoursParent(t *testing.T) {
	a := &ClaudeCLIAdvisor{TimeoutSeconds: -1}
	parent, pcancel := context.WithTimeout(context.Background(), time.Minute)
	defer pcancel()
	ctx, cancel := a.applyTimeout(parent)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("親の deadline が失われている — 停止しても advisor が残る")
	}
}

func TestGenerate_Success(t *testing.T) {
	a := newTestAdvisor(t, "config_id: x\nstrategy_name: bnf_reversion\nmode: paper_config\nentry:\n  direction: buy_only\n", 0)
	run, err := a.Generate(context.Background(), summary())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Status != port.AdvisorRunSuccess || !run.Status.Promotable() {
		t.Fatalf("want success/promotable, got %s", run.Status)
	}
	if len(run.ParsedYAML) == 0 || len(run.InputJSON) == 0 {
		t.Fatalf("missing payloads: parsed=%d input=%d", len(run.ParsedYAML), len(run.InputJSON))
	}
}

func TestGenerate_EmptyStdout(t *testing.T) {
	a := newTestAdvisor(t, "", 0)
	run, _ := a.Generate(context.Background(), summary())
	if run.Status != port.AdvisorRunEmpty || run.Status.Promotable() {
		t.Fatalf("want empty/non-promotable, got %s", run.Status)
	}
}

func TestGenerate_UsageLimit_NotRetriedNotPromotable(t *testing.T) {
	a := newTestAdvisor(t, "You've hit your session limit · resets 4am", 1)
	run, _ := a.Generate(context.Background(), summary())
	if run.Status != port.AdvisorRunCLIError || !run.UsageLimited {
		t.Fatalf("want cli_error+usage_limited, got status=%s usage=%v", run.Status, run.UsageLimited)
	}
	if run.Status.Promotable() {
		t.Fatal("usage limit must not be promotable")
	}
}

func TestGenerate_ParseError_OnGarbage(t *testing.T) {
	a := newTestAdvisor(t, "here is some prose with no config at all\n", 0)
	run, _ := a.Generate(context.Background(), summary())
	if run.Status != port.AdvisorRunParseError {
		t.Fatalf("want parse_error, got %s", run.Status)
	}
}

func TestGenerate_ArchivesRawOutput(t *testing.T) {
	dir := t.TempDir()
	a := newTestAdvisor(t, "config_id: x\nstrategy_name: bnf_reversion\nmode: paper_config\n", 0)
	a.OutputDir = dir
	if _, err := a.Generate(context.Background(), summary()); err != nil {
		t.Fatal(err)
	}
	got := names(mustReadDir(t, dir))
	// both sides of the exchange must be archived so the decision is reproducible
	if !hasSuffix(got, ".input.json") {
		t.Fatalf("success run must archive the LLM input, got %v", got)
	}
	if !hasSuffix(got, ".yaml") || hasSuffix(got, ".fail.yaml") {
		t.Fatalf("success run must archive a .yaml (not .fail.yaml), got %v", got)
	}

	dir2 := t.TempDir()
	b := newTestAdvisor(t, "just prose, no config", 0)
	b.OutputDir = dir2
	if _, err := b.Generate(context.Background(), summary()); err != nil {
		t.Fatal(err)
	}
	got2 := names(mustReadDir(t, dir2))
	if !hasSuffix(got2, ".fail.yaml") || !hasSuffix(got2, ".input.json") {
		t.Fatalf("failed run must archive input + .fail.yaml, got %v", got2)
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	fs, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func hasSuffix(names []string, suffix string) bool {
	for _, n := range names {
		if strings.HasSuffix(n, suffix) {
			return true
		}
	}
	return false
}

func names(fs []os.DirEntry) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name()
	}
	return out
}

func TestGenerate_Timeout(t *testing.T) {
	a := newTestAdvisor(t, "config_id: x\n", 0)
	// exec that sleeps past the deadline
	a.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "5")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	run, _ := a.Generate(ctx, summary())
	if run.Status != port.AdvisorRunTimeout {
		t.Fatalf("want timeout, got %s", run.Status)
	}
}
