package session

import (
	"stockbot/backend/internal/domain/clock"
	"testing"
	"time"
)

func tokyoHours() TradingHours {
	return TradingHours{
		TZ: clock.JST,
		Sessions: []Window{
			{Start: "09:00", End: "11:30"},
			{Start: "12:30", End: "15:00"},
		},
		EntryCutoff: "14:55",
		ForceFlatAt: "14:50",
	}
}

func at(h, m int) time.Time {
	return time.Date(2026, 6, 17, h, m, 0, 0, clock.JST)
}

func TestInTradingHours(t *testing.T) {
	th := tokyoHours()
	cases := []struct {
		h, m int
		want bool
	}{
		{8, 59, false},  // pre-open
		{9, 0, true},    // open
		{11, 29, true},  // 前場 end-1
		{11, 30, false}, // lunch starts
		{12, 0, false},  // lunch
		{12, 30, true},  // 後場 open
		{14, 59, true},  // last minute
		{15, 0, false},  // close
		{16, 0, false},  // after
	}
	for _, tc := range cases {
		if got := th.InTradingHours(at(tc.h, tc.m)); got != tc.want {
			t.Fatalf("InTradingHours(%02d:%02d) = %v, want %v", tc.h, tc.m, got, tc.want)
		}
	}
}

func TestCutoffAndClose(t *testing.T) {
	th := tokyoHours()
	if th.IsAfterEntryCutoff(at(14, 54)) {
		t.Fatal("14:54 should be before cutoff")
	}
	if !th.IsAfterEntryCutoff(at(14, 55)) {
		t.Fatal("14:55 should be at/after cutoff")
	}
	if th.IsNearClose(at(14, 49)) {
		t.Fatal("14:49 should not be near close")
	}
	if !th.IsNearClose(at(14, 50)) {
		t.Fatal("14:50 should be near close")
	}
}

func TestNonTradingDays(t *testing.T) {
	th := tokyoHours()
	th.Holidays = map[string]struct{}{"2026-07-20": {}} // 海の日 (Monday)

	// 2026-06-20 = Saturday, 2026-06-21 = Sunday: always closed.
	sat := time.Date(2026, 6, 20, 10, 0, 0, 0, clock.JST)
	sun := time.Date(2026, 6, 21, 10, 0, 0, 0, clock.JST)
	if th.InTradingHours(sat) || th.InTradingHours(sun) {
		t.Fatal("weekends must never be trading hours")
	}
	if th.IsTradingDay(sat) || th.IsTradingDay(sun) {
		t.Fatal("weekends must not be trading days")
	}

	// Holiday Monday: closed; the following Tuesday: open.
	holiday := time.Date(2026, 7, 20, 10, 0, 0, 0, clock.JST)
	nextDay := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	if th.InTradingHours(holiday) {
		t.Fatal("a listed holiday must not be trading hours")
	}
	if !th.InTradingHours(nextDay) {
		t.Fatal("a regular weekday must be trading hours")
	}
}

func TestCalendarHorizonFailClose(t *testing.T) {
	th := tokyoHours()
	th.CalendarThrough = time.Date(2026, 12, 31, 0, 0, 0, 0, clock.JST)

	// A regular weekday INSIDE the horizon is a trading day.
	inside := time.Date(2026, 12, 31, 10, 0, 0, 0, clock.JST) // Thursday, last covered day
	if !th.IsTradingDay(inside) {
		t.Fatal("2026-12-31 (covered, weekday) must be a trading day")
	}
	// 週末でも祝日でもない平日でも、horizon の外なら fail-close。
	beyond := time.Date(2027, 1, 4, 10, 0, 0, 0, clock.JST) // Monday, uncovered
	if th.IsTradingDay(beyond) {
		t.Fatal("2027-01-04 is past the calendar horizon and must fail-close (not trade blind)")
	}
	if th.InTradingHours(beyond) {
		t.Fatal("beyond-horizon dates must never be in trading hours")
	}
	// Zero horizon (fixtures) imposes no limit.
	th.CalendarThrough = time.Time{}
	if !th.IsTradingDay(beyond) {
		t.Fatal("zero CalendarThrough must impose no horizon limit")
	}
}

// 寄り前ウォームアップ窓は取引時間ではなく advisor の寄り前判断専用 — エントリー可否には使わない。
func TestInPreOpenWindow(t *testing.T) {
	th := tokyoHours()
	lead := 15 * time.Minute
	if !th.InPreOpenWindow(at(8, 50), lead) {
		t.Fatal("08:50 は寄り前15分窓の中(true であるべき)")
	}
	if !th.InPreOpenWindow(at(8, 45), lead) {
		t.Fatal("08:45 ちょうどは窓に含む(下側境界は inclusive)")
	}
	if !th.InPreOpenWindow(at(8, 59), lead) {
		t.Fatal("08:59 は窓の中(上側境界は 09:00 の直前まで)")
	}
	if th.InPreOpenWindow(at(8, 44), lead) {
		t.Fatal("08:44 は窓の外(15分より前)")
	}
	if th.InPreOpenWindow(at(9, 0), lead) {
		t.Fatal("09:00 は場中であって寄り前ではない")
	}
	if th.InPreOpenWindow(at(12, 20), lead) {
		t.Fatal("昼休みは寄り前窓ではない(第1セッションの前だけ)")
	}
	if th.InPreOpenWindow(at(8, 50), 0) {
		t.Fatal("lead=0 は窓なし(既定 OFF)")
	}
	sat := time.Date(2026, 6, 20, 8, 50, 0, 0, clock.JST)
	if th.InPreOpenWindow(sat, lead) {
		t.Fatal("土曜は取引日でないので寄り前窓もない")
	}
}

func TestMinutesUntilClose(t *testing.T) {
	th := tokyoHours()
	if got := th.MinutesUntilClose(at(14, 0)); got != 60 {
		t.Fatalf("MinutesUntilClose(14:00) = %d, want 60", got)
	}
	if got := th.MinutesUntilClose(at(15, 30)); got != -30 {
		t.Fatalf("MinutesUntilClose(15:30) = %d, want -30", got)
	}
}

// 守りの注文期日を「N 営業日先」で置くための日付計算。土日と休場日を飛ばす。
// 立花の sOrderExpireDay は 0(当日)か YYYYMMDD で、上限は 10 営業日。
// これが無いと守りは当日失効し、多日保有の建玉は**2日目に裸になる**。
func TestNthTradingDayFrom(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	th := TradingHours{
		TZ: jst,
		Holidays: map[string]struct{}{
			"2026-08-17": {}, // 月曜を休場に仕立てる
		},
		CalendarThrough: time.Date(2026, 12, 31, 0, 0, 0, 0, jst),
	}
	thu := time.Date(2026, 8, 13, 12, 45, 0, 0, jst) // 木曜

	for _, c := range []struct {
		n    int
		want string
	}{
		{1, "2026-08-14"}, // 金
		{2, "2026-08-18"}, // 土日と休場の月曜を飛ばして火
		{3, "2026-08-19"},
		{0, "2026-08-13"}, // 0 = 当日(当日が取引日ならそのまま)
	} {
		got := th.NthTradingDayFrom(thu, c.n)
		if got.Format("2006-01-02") != c.want {
			t.Errorf("NthTradingDayFrom(木, %d) = %s, want %s", c.n, got.Format("2006-01-02"), c.want)
		}
	}

	// 🛑 カレンダーが尽きたら zero を返す。「たぶん営業日だろう」で期日を置かない
	// (未更新カレンダーで盲目的に取引しない fail-close と同じ姿勢)。
	short := th
	short.CalendarThrough = time.Date(2026, 8, 14, 0, 0, 0, 0, jst)
	if got := short.NthTradingDayFrom(thu, 5); !got.IsZero() {
		t.Errorf("カレンダー期限切れで %s を返した — zero であるべき", got.Format("2006-01-02"))
	}
}

// DayStart is the single venue-midnight helper (was re-implemented inline in
// session, snapshot, bundle and csv_through — item 8 of the structural refactor).
func TestDayStart(t *testing.T) {
	jst := clock.JST
	th := TradingHours{TZ: jst}
	in := time.Date(2026, 9, 4, 23, 30, 0, 0, time.UTC) // = 09-05 08:30 JST
	if got := th.DayStart(in); !got.Equal(time.Date(2026, 9, 5, 0, 0, 0, 0, jst)) {
		t.Fatalf("DayStart = %v, want 2026-09-05 00:00 JST", got)
	}
	if got := (TradingHours{}).DayStart(in); !got.Equal(time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("TZ nil must fall back to UTC midnight, got %v", got)
	}
}

func TestPrevTradingDay(t *testing.T) {
	jst := clock.JST
	th := TradingHours{TZ: jst, Holidays: map[string]struct{}{"2026-09-18": {}}}
	// 09-21(月)から遡る: 20(日)19(土)18(祝) を飛ばして 17(木)。
	got, ok := th.PrevTradingDay(time.Date(2026, 9, 21, 10, 0, 0, 0, jst), 10)
	if !ok || got.Format("2006-01-02") != "2026-09-17" {
		t.Fatalf("PrevTradingDay = %v ok=%v, want 2026-09-17", got, ok)
	}
	if _, ok := th.PrevTradingDay(time.Date(2026, 9, 21, 10, 0, 0, 0, jst), 2); ok {
		t.Fatal("lookback 2 days cannot reach a trading day — must report ok=false, not guess")
	}
}
