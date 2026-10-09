package gonogo

import (
	"os"
	"path/filepath"
	"testing"
)

// 片方の実行中にもう片方を起動しても走らない。
func TestLock_SecondAcquireFailsWhileHeld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run", "gonogo.lock")
	release, ok, err := AcquireLock(p)
	if err != nil || !ok {
		t.Fatalf("1 本目: ok=%v err=%v", ok, err)
	}
	if _, ok2, err := AcquireLock(p); err != nil || ok2 {
		t.Fatalf("2 本目が取れた: ok=%v err=%v", ok2, err)
	}
	release()
	release2, ok3, err := AcquireLock(p)
	if err != nil || !ok3 {
		t.Fatalf("解放後に取れない: ok=%v err=%v", ok3, err)
	}
	release2()
}

// pid が死んでいる古いロックは取り直す。
func TestLock_TakesOverStaleLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gonogo.lock")
	if err := os.WriteFile(p, []byte("999999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, ok, err := AcquireLock(p)
	if err != nil || !ok {
		t.Fatalf("古いロックを取り直せない: ok=%v err=%v", ok, err)
	}
	release()
}
