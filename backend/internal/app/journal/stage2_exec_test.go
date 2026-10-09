package journal

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeCLI は claude の代わりに置く実行可能ファイル。RunStage2 は引数を固定で渡すので、
// 引数を無視して stdin を捨て、決められた出力を返すだけのスクリプトでよい。
func fakeCLI(t *testing.T, body string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sh が要る")
	}
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\ncat > /dev/null\n"
	if body != "" {
		script += "cat <<'EOF'\n" + body + "\nEOF\n"
	}
	script += "exit " + string(rune('0'+exitCode)) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// 🛑 **CLI が落ちた日の `.md` を作らない。** 日記は append-only(O_EXCL)なので、
// 空や壊れた総評を置いた日は**二度と書き直せない**。単体では writeExclusive を
// 検査しているが、経路として通っていることをここで固定する。
func TestRunStage2_FailedCLILeavesTheDayWritable(t *testing.T) {
	out := t.TempDir()
	cli := fakeCLI(t, "", 3)
	if _, err := RunStage2(context.Background(), cli, out, "2026-08-14", []byte(`{}`), 30*time.Second); err == nil {
		t.Fatal("CLI 失敗を成功として扱った")
	}
	if _, err := os.Stat(filepath.Join(out, "2026-08-14.md")); !os.IsNotExist(err) {
		t.Fatal("失敗したのに .md ができている(その日は二度と書けなくなる)")
	}
}

// 総評の体をなしていない出力(usage limit 等)も同じく固定しない。
func TestRunStage2_NonReviewOutputIsNotWritten(t *testing.T) {
	out := t.TempDir()
	cli := fakeCLI(t, `{"type":"result","result":"Claude usage limit reached."}`, 0)
	if _, err := RunStage2(context.Background(), cli, out, "2026-08-14", []byte(`{}`), 30*time.Second); err == nil {
		t.Fatal("非総評テキストを総評として受け入れた")
	}
	if _, err := os.Stat(filepath.Join(out, "2026-08-14.md")); !os.IsNotExist(err) {
		t.Fatal("非総評を .md として固定した")
	}
}

// 成功したら本文と provenance(世代)を書き、同じ日を二度は書かない。
func TestRunStage2_WritesOnceWithProvenance(t *testing.T) {
	out := t.TempDir()
	cli := fakeCLI(t, `{"type":"result","result":"これは日記であってエッジの証拠ではない。","modelUsage":{"claude-opus-5":{}}}`, 0)

	path, err := RunStage2(context.Background(), cli, out, "2026-08-14", []byte(`{"n":1}`), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "model=claude-opus-5") {
		t.Fatalf("世代が記録されていない: %.120q", body)
	}
	if _, err := RunStage2(context.Background(), cli, out, "2026-08-14", []byte(`{"n":1}`), 30*time.Second); err == nil {
		t.Fatal("同じ日を二度書いた(append-only が壊れている)")
	}
}
