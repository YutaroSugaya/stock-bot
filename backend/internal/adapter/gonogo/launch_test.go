package gonogo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/port"
)

// fakeBin は引数と起動経路の env をファイルに書くだけの gonogo の代役。
func fakeBin(t *testing.T, dir string) (bin, out string) {
	t.Helper()
	out = filepath.Join(dir, "called")
	bin = filepath.Join(dir, "gonogo")
	script := "#!/bin/sh\necho \"$* trigger=$STOCKBOT_GONOGO_TRIGGER\" > '" + out + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, out
}

func waitFile(t *testing.T, p string) string {
	t.Helper()
	// 並列実行で負荷が高いと /bin/sh のスタブが 2 秒で起動しないことがある。6 秒まで待つ。
	for range 300 {
		if raw, err := os.ReadFile(p); err == nil && len(raw) > 0 {
			return strings.TrimSpace(string(raw))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s が書かれない(起動していない)", p)
	return ""
}

// 画面のボタンからの起動: 指定の銘柄だけを、起動経路 button として別プロセスで判定する(待たずに戻る)。
func TestLauncher_StartsGonogoForSymbols(t *testing.T) {
	dir := t.TempDir()
	bin, out := fakeBin(t, dir)
	l := &Launcher{Bin: bin, LogPath: filepath.Join(dir, "logs", "gonogo.log"), LockPath: filepath.Join(dir, "run", "gonogo.lock")}
	if err := l.Launch([]string{"6594", "7203"}); err != nil {
		t.Fatal(err)
	}
	if got := waitFile(t, out); got != "-symbols 6594,7203 trigger=button" {
		t.Fatalf("起動の引数 = %q", got)
	}
}

// 判定が実行中(ロックを生きたプロセスが持つ)なら起動しない。
func TestLauncher_RefusesWhileRunning(t *testing.T) {
	dir := t.TempDir()
	bin, out := fakeBin(t, dir)
	lock := filepath.Join(dir, "run", "gonogo.lock")
	release, ok, err := AcquireLock(lock)
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer release()
	l := &Launcher{Bin: bin, LogPath: filepath.Join(dir, "gonogo.log"), LockPath: lock}
	if err := l.Launch([]string{"6594"}); !errors.Is(err, port.ErrGoNoGoRunning) {
		t.Fatalf("err = %v, want ErrGoNoGoRunning", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("実行中なのに起動した")
	}
}

// バイナリが無い(catchup がまだ配っていない)ならエラーで返す。
func TestLauncher_MissingBinary(t *testing.T) {
	dir := t.TempDir()
	l := &Launcher{Bin: filepath.Join(dir, "nope"), LogPath: filepath.Join(dir, "gonogo.log"), LockPath: filepath.Join(dir, "gonogo.lock")}
	if err := l.Launch([]string{"6594"}); err == nil {
		t.Fatal("バイナリが無いのに成功した")
	}
}
