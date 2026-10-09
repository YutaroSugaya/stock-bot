package session

import (
	"testing"
	"time"
)

func jstLoc(t *testing.T) *time.Location {
	t.Helper()
	tz, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	return tz
}

// 2026-08-11 は山の日(休場)。hard_limits と同じ形の休場テーブルを最小で作る。
func businessDaysHours(t *testing.T) TradingHours {
	t.Helper()
	return TradingHours{
		TZ: jstLoc(t),
		Holidays: map[string]struct{}{
			"2026-08-11": {}, // 山の日
		},
		// 実運用と同じく horizon を持たせる。AddBusinessDays はこれを跨いでも
		// 週末だけは確実に飛ばせなければならない(下のケースで固定する)。
		CalendarThrough: time.Date(2026, 12, 31, 0, 0, 0, 0, jstLoc(t)),
	}
}

func TestAddBusinessDaysSkipsWeekendsAndHolidays(t *testing.T) {
	th := businessDaysHours(t)
	tz := th.TZ
	cases := []struct {
		name string
		from time.Time
		n    int
		want time.Time
	}{
		{
			name: "月曜から1営業日は火曜",
			from: time.Date(2026, 8, 17, 9, 30, 0, 0, tz),
			n:    1,
			want: time.Date(2026, 8, 18, 9, 30, 0, 0, tz),
		},
		{
			name: "金曜から1営業日は月曜(週末を飛ばす)",
			from: time.Date(2026, 8, 21, 9, 30, 0, 0, tz),
			n:    1,
			want: time.Date(2026, 8, 24, 9, 30, 0, 0, tz),
		},
		{
			name: "山の日を飛ばす(08-10 月 → 2営業日 → 08-13 木)",
			from: time.Date(2026, 8, 10, 9, 30, 0, 0, tz),
			n:    2,
			want: time.Date(2026, 8, 13, 9, 30, 0, 0, tz),
		},
		{
			name: "n=0 は同時刻をそのまま返す",
			from: time.Date(2026, 8, 17, 14, 5, 0, 0, tz),
			n:    0,
			want: time.Date(2026, 8, 17, 14, 5, 0, 0, tz),
		},
		{
			name: "負値も同時刻をそのまま返す(期限なしの扱いは呼び手が決める)",
			from: time.Date(2026, 8, 17, 14, 5, 0, 0, tz),
			n:    -3,
			want: time.Date(2026, 8, 17, 14, 5, 0, 0, tz),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := th.AddBusinessDays(c.from, c.n)
			if !got.Equal(c.want) {
				t.Fatalf("AddBusinessDays(%s, %d) = %s, want %s",
					c.from.Format(time.RFC3339), c.n, got.Format(time.RFC3339), c.want.Format(time.RFC3339))
			}
		})
	}
}

// 🛑 IsTradingDay は CalendarThrough を過ぎると **全部 false** に倒れる(未更新カレンダーで
// 取引しない fail-close)。AddBusinessDays がそれを使っていると、126 営業日先を求める
// abs_momentum_v2 の MaxHold で「取引日が永久に見つからない」= 無限ループになる。
// horizon の外でも週末だけは確実に飛ばして必ず終わること。
func TestAddBusinessDaysTerminatesBeyondCalendarHorizon(t *testing.T) {
	th := businessDaysHours(t)
	tz := th.TZ
	from := time.Date(2026, 8, 24, 9, 0, 0, 0, tz) // 月曜
	got := th.AddBusinessDays(from, 126)
	if !got.After(from) {
		t.Fatalf("126 営業日先が過去/同日になった: %s", got.Format(time.RFC3339))
	}
	// 126 営業日 ≈ 176 暦日(週末 50 日ぶん)。祝日を数えない分だけ短めに出るので
	// 下限だけを固定する(上限は 126*7/5 + 余裕)。
	days := int(got.Sub(from).Hours() / 24)
	if days < 176 || days > 200 {
		t.Fatalf("126 営業日 = %d 暦日は不自然(週末を飛ばしていない/飛ばしすぎ)", days)
	}
	if wd := got.In(tz).Weekday(); wd == time.Saturday || wd == time.Sunday {
		t.Fatalf("着地が週末: %s", got.Format(time.RFC3339))
	}
	// 時刻は保存される(MaxHoldMinutes は建玉時刻からの分数なので、時刻がずれると
	// 期限が場外へ落ちる)。
	if h, m := got.In(tz).Hour(), got.In(tz).Minute(); h != 9 || m != 0 {
		t.Fatalf("時刻が保存されていない: %02d:%02d", h, m)
	}
}

// 差が丸1日の倍数であること = MaxHoldMinutes が「時刻に依らず日付だけで決まる」保証。
// これが崩れると config の Fingerprint が毎ラウンド変わり、strategy_configs が
// 1日 200銘柄 × 270ラウンド ぶん膨れる。
func TestAddBusinessDaysDifferenceIsWholeDaysRegardlessOfTimeOfDay(t *testing.T) {
	th := businessDaysHours(t)
	tz := th.TZ
	for _, hm := range [][2]int{{9, 0}, {11, 27}, {14, 59}} {
		from := time.Date(2026, 8, 10, hm[0], hm[1], 0, 0, tz)
		got := th.AddBusinessDays(from, 7)
		mins := int(got.Sub(from).Minutes())
		if mins%1440 != 0 {
			t.Fatalf("%02d:%02d 始点で差が丸1日の倍数でない: %d 分", hm[0], hm[1], mins)
		}
	}
}
