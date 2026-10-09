package session

import (
	"stockbot/backend/internal/domain/clock"
	"testing"
	"time"
)

// 「今日の日足はまだ要るか」を答える。既に持つバーを再ポーリングしない(立花から高負荷と指摘された経路)。
func TestLastClosedTradingDay(t *testing.T) {
	th := tokyoHours()
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		// 2026-06-17 は水曜。
		{"場中は前営業日まで", time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST), "2026-06-16"},
		{"引け前1分もまだ前営業日", time.Date(2026, 6, 17, 14, 59, 0, 0, clock.JST), "2026-06-16"},
		{"引け後は当日", time.Date(2026, 6, 17, 15, 0, 0, 0, clock.JST), "2026-06-17"},
		{"夜間は当日", time.Date(2026, 6, 17, 23, 0, 0, 0, clock.JST), "2026-06-17"},
		{"寄り前は前営業日", time.Date(2026, 6, 17, 8, 0, 0, 0, clock.JST), "2026-06-16"},
		// 2026-06-20 は土曜、6/21 は日曜 → 直近の引けは金曜 6/19。
		{"土曜は金曜", time.Date(2026, 6, 20, 12, 0, 0, 0, clock.JST), "2026-06-19"},
		{"日曜は金曜", time.Date(2026, 6, 21, 12, 0, 0, 0, clock.JST), "2026-06-19"},
		{"月曜の寄り前は金曜", time.Date(2026, 6, 22, 8, 0, 0, 0, clock.JST), "2026-06-19"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := th.LastClosedTradingDay(tc.now)
			if !ok {
				t.Fatalf("ok=false, want %s", tc.want)
			}
			if got != tc.want {
				t.Errorf("LastClosedTradingDay = %s, want %s", got, tc.want)
			}
		})
	}
}

// 祝日はスキップする(その日の日足は存在しない)。
func TestLastClosedTradingDaySkipsHolidays(t *testing.T) {
	th := tokyoHours()
	th.Holidays = map[string]struct{}{"2026-06-16": {}, "2026-06-15": {}}

	got, ok := th.LastClosedTradingDay(time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST))
	if !ok || got != "2026-06-12" { // 6/13-14 は土日、6/15-16 は祝日
		t.Fatalf("LastClosedTradingDay = %q (ok=%v), want 2026-06-12", got, ok)
	}
}

// カレンダー horizon を過ぎたら fail-close(「分からない」を「全部取引日でない」と誤読して無限に遡らない)。
func TestLastClosedTradingDayFailsClosedPastCalendar(t *testing.T) {
	th := tokyoHours()
	th.CalendarThrough = time.Date(2026, 6, 10, 0, 0, 0, 0, clock.JST)

	if got, ok := th.LastClosedTradingDay(time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)); ok {
		t.Fatalf("stale calendar must not answer, got %q", got)
	}
}
