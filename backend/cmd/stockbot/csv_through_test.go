package main

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/session"
)

func csvHours() session.TradingHours {
	return session.TradingHours{
		TZ:          clock.JST,
		Sessions:    []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:00"}},
		EntryCutoff: "14:55",
		ForceFlatAt: "14:50",
	}
}

func jstTime(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, clock.JST)
}

// CSV の基準日は DB の基準日(LastClosedTradingDay)より**1営業日うしろ**。
// fetch-daily が未確定の当日足を取り込まないため。この差を織り込まないと引け後から
// 翌朝までずっと全銘柄が「遅れている」と鳴り、常態化した誤報が本物の停止を隠す。
func TestCSVFreshThroughExcludesTodaysUnconfirmedBar(t *testing.T) {
	th := csvHours()
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		// 2026-08-05 は水曜。
		{"場中は前営業日(DB と同じ)", jstTime(2026, 8, 5, 10, 0), "2026-08-04"},
		{"引け直後は当日を基準にしない", jstTime(2026, 8, 5, 15, 30), "2026-08-04"},
		{"夜間もまだ当日足は入らない", jstTime(2026, 8, 5, 23, 0), "2026-08-04"},
		{"翌朝の寄り前は前日ぶんを要求する", jstTime(2026, 8, 6, 8, 0), "2026-08-05"},
		// 2026-08-08 は土曜、8/9 は日曜。金曜 8/7 の引け後〜週末は金曜ぶんを飛ばして木曜。
		{"金曜の引け後は木曜まで", jstTime(2026, 8, 7, 16, 0), "2026-08-06"},
		{"土曜は金曜ぶんを要求する", jstTime(2026, 8, 8, 12, 0), "2026-08-07"},
		{"月曜の寄り前も金曜まで", jstTime(2026, 8, 10, 8, 0), "2026-08-07"},
		// 🛑 **日付が変わっても、前営業日の足は朝ジョブ(07:00〜)が入れるまで存在しない。**
		// 00:00 に基準日だけ進めると、bot を上げっぱなしにした晩は毎回 0時〜8時のあいだ
		// 全銘柄が「遅れている」と鳴る(実測)。常時鳴る警告は本物の停止を隠す。
		{"金曜の深夜〜土曜未明はまだ木曜まで", jstTime(2026, 8, 8, 2, 0), "2026-08-06"},
		{"平日の未明も前々営業日まで", jstTime(2026, 8, 6, 3, 0), "2026-08-04"},
		{"取得が終わる 08:00 から前営業日を要求する", jstTime(2026, 8, 8, 8, 0), "2026-08-07"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := csvFreshThrough(th, tc.now); got != tc.want {
				t.Errorf("csvFreshThrough = %q, want %q", got, tc.want)
			}
		})
	}
}

// カレンダー失効時は空を返して鮮度監視を黙らせる(取引自体は session 側が止める)。
func TestCSVFreshThroughIsEmptyWhenCalendarExpired(t *testing.T) {
	th := csvHours()
	th.CalendarThrough = jstTime(2026, 1, 1, 0, 0)
	if got := csvFreshThrough(th, jstTime(2026, 8, 5, 23, 0)); got != "" {
		t.Errorf("カレンダー失効で %q を返した(空であるべき)", got)
	}
}
