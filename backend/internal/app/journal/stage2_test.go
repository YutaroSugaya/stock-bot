package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// JSON は**そのまま**渡す。要約した時点で「LLM に数字を作らせない」が崩れる。
func TestBuildStage2Prompt_EmbedsPacketVerbatim(t *testing.T) {
	got := buildStage2Prompt("RULES", []byte(`{"n":3,"net_jpy":-7250}`))
	if !strings.Contains(got, "RULES") || !strings.Contains(got, `"net_jpy":-7250`) {
		t.Fatalf("prompt = %q", got)
	}
	if !strings.Contains(got, "## Input JSON") {
		t.Fatalf("入力の見出しが無い: %q", got)
	}
}

// 🛑 append-only は **カーネルに保証させる**(O_EXCL)。stat → write は TOCTOU で
// 上書きに倒れる。書き換えを許すと「あの時こう思っていた」の後付けが効く。
func TestWriteExclusive_RefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "2026-08-14.md")
	if err := os.WriteFile(p, []byte("既存"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeExclusive(p, "新しい総評")
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("上書きを拒否していない: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "既存" {
		t.Fatalf("既存の総評が壊れた: %q", b)
	}
}

// 🚨 CLI は **exit 0 でも** usage limit / infra のメッセージを返すことがある(advisor の
// 実測)。append-only なので、それを日記として書いた日は二度と総評を書けなくなる。
func TestStage2Text_RejectsNonReviewOutput(t *testing.T) {
	for _, blob := range []string{
		`{"result":"Claude usage limit reached. Try again later.","model":"claude-opus-5"}`,
		`Claude usage limit reached`,
		`{"result":"","model":"x"}`,
	} {
		if _, _, err := stage2Text([]byte(blob)); err == nil {
			t.Fatalf("総評でない応答を受け入れた: %q", blob)
		}
	}
}

// envelope から本文とモデル世代を取り出す(どの世代が書いた日記かを残す)。
func TestStage2Text_ExtractsResultAndModel(t *testing.T) {
	blob := `{"result":"> ⚠ これは日記であってエッジの証拠ではない\n\n## 数字\n…","model":"claude-opus-5"}`
	text, model, err := stage2Text([]byte(blob))
	if err != nil {
		t.Fatalf("stage2Text: %v", err)
	}
	if !strings.HasPrefix(text, "> ⚠") || model != "claude-opus-5" {
		t.Fatalf("text=%q model=%q", text, model)
	}
	// envelope でない素のテキストでも本文として通す(モデルは unknown)。
	text, model, err = stage2Text([]byte("> ⚠ これは日記であってエッジの証拠ではない\n本文"))
	if err != nil || model != "unknown" || !strings.Contains(text, "本文") {
		t.Fatalf("素テキスト: text=%q model=%q err=%v", text, model, err)
	}
}

// 🚨 検索は **allowlist で明示的に渡す**。渡さないと `-p`(非対話)では permission が
// auto-deny され、WebSearch が呼べない = ニュース章の前提が成立しない(実測)。
// claude CLI の `--output-format json` は**トップレベルに `model` を持たない** —
// 世代は `modelUsage` のキーに出る(advisor の splitEnvelope が同じ扱い)。
// 実測: `model=unknown` のまま日記が書かれていた。日記は月を跨いで
// 読み比べる文書で、その間に CLI は上がる。**どの世代が書いたかは残す。**
func TestStage2Text_TakesModelFromModelUsage(t *testing.T) {
	blob := []byte(`{"type":"result","result":"エッジの証拠ではない。本文。",` +
		`"modelUsage":{"claude-opus-5":{"inputTokens":1},"claude-haiku-4-5":{"inputTokens":2}}}`)
	_, model, err := stage2Text(blob)
	if err != nil {
		t.Fatal(err)
	}
	// map の反復順は不定 — 記録を決定論にするため sort する。
	if model != "claude-haiku-4-5,claude-opus-5" {
		t.Fatalf("model = %q", model)
	}
}

func TestStage2ToolArgs_AllowsWebSearchExplicitly(t *testing.T) {
	joined := strings.Join(stage2ToolArgs, " ")
	if !strings.Contains(joined, "--tools WebSearch") || !strings.Contains(joined, "--allowed-tools WebSearch") {
		t.Fatalf("tool args = %q — 非対話では明示的に許可しないと呼べない", joined)
	}
}

// 書込・実行系と**リポジトリを読む手段**は落とす。CWD 配下を Read できると
// 「JSON に無い数字を書かない」が道具レベルで支えられなくなる。
func TestStage2DisallowedTools_BlocksWritesAndReads(t *testing.T) {
	for _, tool := range []string{"Bash", "Edit", "Write", "NotebookEdit", "Read", "Glob", "Grep", "Artifact"} {
		if !strings.Contains(stage2DisallowedTools, tool) {
			t.Errorf("%s が禁止リストに無い", tool)
		}
	}
	if strings.Contains(stage2DisallowedTools, "WebSearch") {
		t.Error("WebSearch まで落とすとニュース章が書けない")
	}
}

// モデル指定に**版番号を焼かない**(advisor と同じ規律。opus = 最新の Opus)。
func TestStage2ModelArgs_NoVersionPin(t *testing.T) {
	for _, a := range stage2ModelArgs {
		if strings.Contains(a, "claude-") || strings.Contains(a, "-2026") {
			t.Fatalf("版番号が焼かれている: %q", a)
		}
	}
}

// プロンプトは binary に焼く(launchd はビルド済みバイナリを repo 非依存で叩く)。
// 焼いた内容に規律が入っていること自体も杭にする。
func TestStage2Prompt_Embedded(t *testing.T) {
	for _, want := range []string{"エッジの証拠ではない", "原因の帰属", "generated_at", "比較"} {
		if !strings.Contains(stage2Prompt, want) {
			t.Errorf("プロンプトに %q が無い", want)
		}
	}
}
