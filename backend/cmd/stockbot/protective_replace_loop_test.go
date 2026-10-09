package main

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/session"
)

// 🚨 **守りの期日は「取消 → 再発注」でしか延ばせない**。
// 立花の sOrderExpireDay「10営業日迄」は**発注日起点**なので、訂正では天井を超えられ
// ない。だから自動化するのは訂正ループではなく置き直しループ。
//
// 🛑 **撃ってよいのは寄り前だけ。**取消と再発注の間は守りが完全に消えるので、
// 値が動く時間帯に窓を開けない。引け後でなく寄り前なのは、失敗したときに人間が
// 寄りまでに手当てできる時間が残るから(引け後に失敗すると裸で一晩持ち越す)。
func replaceLoopHours() session.TradingHours {
	return session.TradingHours{
		TZ:              clock.JST,
		CalendarThrough: time.Date(2026, 12, 31, 0, 0, 0, 0, clock.JST),
		Sessions:        []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:30"}},
	}
}

func TestShouldReplaceProtectiveNow(t *testing.T) {
	h := replaceLoopHours()
	at := func(m, d, hh, mm int) time.Time {
		return time.Date(2026, time.Month(m), d, hh, mm, 0, 0, clock.JST)
	}
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		// 2026-08-25 は火曜(営業日)
		{"寄り前の窓(07:00)", at(8, 25, 7, 0), true},
		{"寄り前の窓(08:30) — 実際に本番で通した時刻", at(8, 25, 8, 30), true},
		{"寄り前の窓の終わり(08:59)", at(8, 25, 8, 59), true},
		// 🛑 場中は絶対に撃たない。守りが消える窓を値動きの中で開けない。
		{"寄り付き(09:00)は撃たない", at(8, 25, 9, 0), false},
		{"場中(10:30)は撃たない", at(8, 25, 10, 30), false},
		{"昼休み(12:00)も撃たない", at(8, 25, 12, 0), false},
		{"引け後(16:00)は撃たない — 失敗すると裸で一晩持ち越す", at(8, 25, 16, 0), false},
		// 🛑 深夜は撃たない。broker のメンテ窓に当たると照会も発注も落ちる。
		{"深夜(03:00)は撃たない", at(8, 25, 3, 0), false},
		{"早朝(06:00)はまだ窓の外", at(8, 25, 6, 0), false},
		// 2026-08-23 は日曜
		{"休場日は撃たない", at(8, 23, 8, 0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldReplaceProtectiveNow(h, c.now); got != c.want {
				t.Fatalf("shouldReplaceProtectiveNow(%s) = %v, want %v", c.now.Format("01-02 15:04"), got, c.want)
			}
		})
	}
}

// 🛑 周期は窓より短くないと、窓を跨いで 1 度も回らない起動が出る。
func TestReplaceProtectiveIntervalFitsInsideThePreOpenWindow(t *testing.T) {
	if replaceProtectiveInterval >= replaceProtectivePreOpenLead {
		t.Fatalf("interval %v >= 窓 %v — 窓の中で 1 度も回らない起動があり得る",
			replaceProtectiveInterval, replaceProtectivePreOpenLead)
	}
}

// 🚨 **起動直後に 1 回撃つ**。
//
// runTicker は最初の 1 回を interval ぶん待つ。守りの置き直しは**寄り前の窓
// (07:00〜09:00)でしか走らない**ので、08:55 に起動すると初回チェックが 09:05 =
// 窓の外になり、**その日は丸ごと逃す**。「make start するだけでよい」という
// 運用前提が、起動時刻によって静かに破れる形になっていた。
//
// Ticks are injected, so the test never waits on a wall-clock timer.
func TestRunTicksLeadingFiresBeforeTheFirstTick(t *testing.T) {
	calls := 0
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runTicksLeading(ctx, ticks, nil, "test", "leading", func() { calls++ })
		close(done)
	}()
	// No tick was ever sent: cancelling must still leave exactly one (leading) call.
	cancel()
	<-done
	if calls != 1 {
		t.Fatalf("leading call count = %d, want 1 (fired before any tick)", calls)
	}
}

// 🛑 先頭で撃った後も周期は保つ(1 回だけの実行にならない)。
func TestRunTicksLeadingKeepsTicking(t *testing.T) {
	calls := make(chan struct{}, 8)
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runTicksLeading(ctx, ticks, nil, "test", "leading", func() { calls <- struct{}{} })
		close(done)
	}()
	<-calls // leading
	for i := 0; i < 3; i++ {
		ticks <- time.Time{} // unbuffered: the send completes only when the loop consumed it
		<-calls
	}
	cancel()
	<-done
	if len(calls) != 0 {
		t.Fatalf("%d extra calls without a tick", len(calls))
	}
}
