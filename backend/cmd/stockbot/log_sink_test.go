package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 🛑 **`make start` だけでログがファイルに残る**。
//
// これまでは `make start 2>&1 | tee ~/.stockbot/logs/bot-$(date +%F).log` を
// 人間が付ける前提だった。実際に再起動で付け忘れが起き、
// **訂正 API の実機初検証のログが1行も残らなかった**。
//
// Makefile は enforcement ファイルで AI が触れないので、**bot 自身が書く**。
// 起動のしかたに依存しないぶん、こちらのほうが確実でもある。
// 🛑 stdout は止めない — 前景で走らせている人間の画面から消してはいけない。
func TestLogSinkWritesTodaysFileAndKeepsStdout(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC)

	w, path, err := openLogSink(dir, now)
	if err != nil {
		t.Fatalf("openLogSink: %v", err)
	}
	defer w.Close()

	if got := filepath.Base(path); got != "bot-2026-08-24.log" {
		t.Errorf("ファイル名 = %q, want bot-2026-08-24.log", got)
	}
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "hello") {
		t.Errorf("ファイルに書けていない: %q", b)
	}
}

// 🛑 **同じ日に再起動しても前のログを消さない。** truncate すると、朝の起動で出た
// エラーを昼の再起動が消してしまう(`tee` はまさにこれを起こす)。追記で開く。
func TestLogSinkAppendsAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC)

	w1, path, err := openLogSink(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w1.Write([]byte("first\n"))
	w1.Close()

	w2, _, err := openLogSink(dir, now.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w2.Write([]byte("second\n"))
	w2.Close()

	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "first") {
		t.Error("再起動で前のログが消えた — 朝のエラーが昼の再起動で失われる")
	}
	if !strings.Contains(string(b), "second") {
		t.Error("再起動後のログが書けていない")
	}
}

// 書けない場所を渡されても **起動を止めない**。ログが残らないことより
// bot が上がらないことのほうが害が大きい。
func TestLogSinkFailsSoftly(t *testing.T) {
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openLogSink(filepath.Join(f, "sub"), time.Now()); err == nil {
		t.Error("書けないのに error を返さない — 呼び手が握り潰す判断をできない")
	}
}
