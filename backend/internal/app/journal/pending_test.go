package journal

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
)

func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 段2 を書くべき日 = 「段1 の JSON はあるが .md が無い」日。**新しい順**に返す
// (古い日で詰まっても新しい日の総評が書けるように)。
//
// 🛑 **当日は含めない。** 当日の日足は翌朝の fetch-daily 待ちなので、引け直後に組んだ
// 段1 は市況が必ず「取得できず」になる。段2 は `O_EXCL` で二度と書き直せないので、
// その日に書くと**市況の無い総評が永久に固定される**。
func TestPendingStage2Days_NewestFirstAndSkipsWrittenAndToday(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "2026-08-12.json")
	touch(t, dir, "2026-08-13.json")
	touch(t, dir, "2026-08-13.md") // 済み
	touch(t, dir, "2026-08-14.json")
	touch(t, dir, "2026-08-11.json")

	asOf := time.Date(2026, 8, 14, 16, 0, 0, 0, clock.JST)
	got := PendingStage2Days(dir, asOf, 5, 30)
	want := []string{"2026-08-12", "2026-08-11"} // 08-14 は当日なので出ない
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// 🛑 引け後に LLM を連打しない。max で打ち切り、lookback より古い日は諦める
// (何日も落ちていた後に何十日ぶんも回すと、その日の総評すら書けなくなる)。
func TestPendingStage2Days_CapsAndLookback(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"2026-07-01", "2026-08-12", "2026-08-13", "2026-08-14"} {
		touch(t, dir, d+".json")
	}
	asOf := time.Date(2026, 8, 14, 16, 0, 0, 0, clock.JST)
	got := PendingStage2Days(dir, asOf, 2, 7)
	if len(got) != 2 || got[0] != "2026-08-13" || got[1] != "2026-08-12" {
		t.Fatalf("got %v, want [2026-08-13 2026-08-12]", got)
	}
	if all := PendingStage2Days(dir, asOf, 10, 7); len(all) != 2 {
		t.Fatalf("lookback 7日の外(2026-07-01)を拾っている: %v", all)
	}
}

// 未来日・当日・壊れたファイル名は返さない。
func TestPendingStage2Days_IgnoresFutureTodayAndJunk(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "2026-08-20.json")
	touch(t, dir, "notes.json")
	touch(t, dir, "2026-8-4.json") // ゼロ埋めでない = 辞書順比較が壊れる形
	touch(t, dir, "2026-08-14.json")
	touch(t, dir, "2026-08-13.json")
	asOf := time.Date(2026, 8, 14, 16, 0, 0, 0, clock.JST)
	got := PendingStage2Days(dir, asOf, 10, 30)
	if len(got) != 1 || got[0] != "2026-08-13" {
		t.Fatalf("got %v, want [2026-08-13]", got)
	}
}

// 🛑 **同じ日を 1 日に何度も LLM に投げない。** `make stop/start` は任意タイミングで
// 打たれる前提で、失敗し続ける日があると再起動のたびに Opus/max を焼くことになる
// (claude の枠は advisor = 測定に効く経路と共有)。試行の印はディスクに残す。
func TestPendingStage2Days_SkipsDaysAlreadyAttemptedToday(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "2026-08-13.json")
	asOf := time.Date(2026, 8, 14, 16, 0, 0, 0, clock.JST)
	if got := PendingStage2Days(dir, asOf, 5, 30); len(got) != 1 {
		t.Fatalf("初回で出ていない: %v", got)
	}
	if err := MarkStage2Attempt(dir, "2026-08-13", asOf); err != nil {
		t.Fatal(err)
	}
	if got := PendingStage2Days(dir, asOf, 5, 30); len(got) != 0 {
		t.Fatalf("同日に再試行しようとしている: %v", got)
	}
	// 翌日はまた試す(一過性の障害で永久に諦めない)。
	next := asOf.AddDate(0, 0, 1)
	if got := PendingStage2Days(dir, next, 5, 30); len(got) != 1 {
		t.Fatalf("翌日に再試行しない: %v", got)
	}
}

// ディレクトリが無い日(初回起動)は空を返す。エラーで bot を騒がせない。
func TestPendingStage2Days_MissingDir(t *testing.T) {
	if got := PendingStage2Days(filepath.Join(t.TempDir(), "nope"), time.Now(), 5, 30); len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}
