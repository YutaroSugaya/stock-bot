package session

import (
	"testing"
	"time"
)

// 多日建玉の時間切れは「建玉日から N 営業日目の引け前(ForceFlatAt)」。
// 暦日 / 同時刻だと、期限が連休や夜間に落ちて連休明けの寄りで成行になる。
func silverWeekHours(t *testing.T) TradingHours {
	t.Helper()
	return TradingHours{
		TZ:          jstLoc(t),
		Sessions:    []Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:30"}},
		ForceFlatAt: "14:50",
		Holidays: map[string]struct{}{
			"2026-09-21": {}, "2026-09-22": {}, "2026-09-23": {},
		},
		CalendarThrough: time.Date(2027, 12, 31, 0, 0, 0, 0, jstLoc(t)),
	}
}

func TestMaxHoldDeadlineLandsOnNthBusinessDayForceFlat(t *testing.T) {
	th := silverWeekHours(t)
	tz := th.TZ
	cases := []struct {
		name string
		from time.Time
		n    int
		want time.Time
	}{
		{"朝の建玉・シルバーウィーク跨ぎ(3186)", time.Date(2026, 9, 14, 9, 1, 24, 0, tz), 10, time.Date(2026, 10, 1, 14, 50, 0, 0, tz)},
		{"引け後の建玉も同じ日付に落ちる", time.Date(2026, 9, 14, 15, 10, 0, 0, tz), 10, time.Date(2026, 10, 1, 14, 50, 0, 0, tz)},
		{"donchian 10 営業日(8604)", time.Date(2026, 9, 7, 10, 20, 24, 0, tz), 10, time.Date(2026, 9, 24, 14, 50, 0, 0, tz)},
		{"金曜の建玉 1 営業日 = 月曜の 14:50", time.Date(2026, 9, 11, 9, 0, 0, 0, tz), 1, time.Date(2026, 9, 14, 14, 50, 0, 0, tz)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := th.MaxHoldDeadline(c.from, c.n)
			if !ok {
				t.Fatal("ok=false: 取引時間が揃っているのに期限を引けの前に揃えなかった")
			}
			if !got.Equal(c.want) {
				t.Fatalf("deadline = %v, want %v", got.In(tz), c.want)
			}
		})
	}
}

// 取引時間を持たない呼び手(backtest の日足リプレイ)は従来どおり「N 営業日後の同時刻」に
// 縮退する。**無期限には縮退させない**(strategy.EvalInput.Hours の注記)。
func TestMaxHoldDeadlineFallsBackWithoutSessionTimes(t *testing.T) {
	th := businessDaysHours(t) // Sessions / ForceFlatAt なし
	from := time.Date(2026, 9, 14, 9, 1, 0, 0, th.TZ)
	got, ok := th.MaxHoldDeadline(from, 3)
	if ok {
		t.Fatal("ok=true: 取引時間が無いのに引け前に揃えた")
	}
	if want := th.AddBusinessDays(from, 3); !got.Equal(want) {
		t.Fatalf("fallback deadline = %v, want %v", got, want)
	}
	if got, _ := th.MaxHoldDeadline(from, 0); !got.Equal(from) {
		t.Fatalf("n=0 は t をそのまま返す: got %v", got)
	}
}
