// Package session は東証の取引時間・休場カレンダー。すべて純粋関数で、caller が now を渡す。
package session

import (
	"fmt"
	"time"
)

type Window struct {
	Start string // "HH:MM" in the configured timezone
	End   string // "HH:MM"
}

type TradingHours struct {
	TZ          *time.Location
	Sessions    []Window
	EntryCutoff string              // "HH:MM"; no new entries at/after this time
	ForceFlatAt string              // "HH:MM"; intraday positions force-closed at/after this
	Holidays    map[string]struct{} // "2006-01-02" dates (venue TZ) the market is closed

	// 休場カレンダーが覆う最終日。これを超えたら期限切れとして IsTradingDay を false に倒す
	// (年次更新の人間コミットまで取引日にしない fail-close)。zero = 制限なし。
	CalendarThrough time.Time
}

// 週末・祝日・カレンダー期限切れのいずれかなら取引日ではない(未更新カレンダーで盲目的に取引しない)。
func (th TradingHours) IsTradingDay(now time.Time) bool {
	tz := th.TZ
	if tz == nil {
		tz = time.UTC
	}
	t := now.In(tz)
	if !th.CalendarCovers(now) {
		return false // stale calendar → fail-close (no trading until 年次更新)
	}
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	if th.Holidays != nil {
		if _, closed := th.Holidays[t.Format("2006-01-02")]; closed {
			return false
		}
	}
	return true
}

// zone is the venue timezone, UTC when unset.
func (th TradingHours) zone() *time.Location {
	if th.TZ == nil {
		return time.UTC
	}
	return th.TZ
}

// DayStart is midnight of t's calendar day in the venue timezone. Every
// "which trading day is this" question anchors here; computing it in UTC puts
// a JST morning on the previous day.
func (th TradingHours) DayStart(t time.Time) time.Time {
	tz := th.zone()
	l := t.In(tz)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, tz)
}

// PrevTradingDay is the latest trading day strictly before from, looking back at
// most maxLookback calendar days. ok=false means none was found within the
// window (a broken calendar) — callers must not guess.
func (th TradingHours) PrevTradingDay(from time.Time, maxLookback int) (time.Time, bool) {
	for i := 1; i <= maxLookback; i++ {
		prev := from.AddDate(0, 0, -i)
		if th.IsTradingDay(prev) {
			return th.DayStart(prev), true
		}
	}
	return time.Time{}, false
}

// horizon は venue TZ の CalendarThrough 当日を含む。未設定(zero)なら常に true。
func (th TradingHours) CalendarCovers(now time.Time) bool {
	if th.CalendarThrough.IsZero() {
		return true
	}
	return !th.DayStart(now).After(th.DayStart(th.CalendarThrough))
}

func minutesOfDay(hhmm string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(hhmm, "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func (th TradingHours) nowMinutes(now time.Time) int {
	tz := th.TZ
	if tz == nil {
		tz = time.UTC
	}
	t := now.In(tz)
	return t.Hour()*60 + t.Minute()
}

func (th TradingHours) InTradingHours(now time.Time) bool {
	if !th.IsTradingDay(now) {
		return false
	}
	cur := th.nowMinutes(now)
	for _, w := range th.Sessions {
		s, ok1 := minutesOfDay(w.Start)
		e, ok2 := minutesOfDay(w.End)
		if !ok1 || !ok2 {
			continue
		}
		if cur >= s && cur < e {
			return true
		}
	}
	return false
}

// 寄り前ウォームアップ窓。取引時間ではなく advisor の寄り前判断専用(lead<=0 は常に false = 既定 OFF)。
func (th TradingHours) InPreOpenWindow(now time.Time, lead time.Duration) bool {
	if lead <= 0 || !th.IsTradingDay(now) || len(th.Sessions) == 0 {
		return false
	}
	openMin, ok := minutesOfDay(th.Sessions[0].Start)
	if !ok {
		return false
	}
	nowMin := th.nowMinutes(now)
	leadMin := int(lead / time.Minute)
	return nowMin >= openMin-leadMin && nowMin < openMin
}

// 新規 entry cutoff(14:55)。risk gate が遅い entry を reject するのに使う。
func (th TradingHours) IsAfterEntryCutoff(now time.Time) bool {
	cut, ok := minutesOfDay(th.EntryCutoff)
	if !ok {
		return false
	}
	return th.nowMinutes(now) >= cut
}

// 引け前フラット化時刻(14:50)。15:00 の固定ペナルティ付き自動決済より前に返済するため。
func (th TradingHours) IsNearClose(now time.Time) bool {
	ff, ok := minutesOfDay(th.ForceFlatAt)
	if !ok {
		return false
	}
	return th.nowMinutes(now) >= ff
}

// 大引け(最終セッション終了)までの分。過ぎていれば負。
func (th TradingHours) MinutesUntilClose(now time.Time) int {
	if len(th.Sessions) == 0 {
		return 0
	}
	last := th.Sessions[len(th.Sessions)-1]
	e, ok := minutesOfDay(last.End)
	if !ok {
		return 0
	}
	return e - th.nowMinutes(now)
}

// 強制フラットまでの分。intraday Signal の MaxHoldMinutes を cap するのに使う(0/負 = 入る余地なし)。
func (th TradingHours) MinutesUntilForceFlat(now time.Time) int {
	ff, ok := minutesOfDay(th.ForceFlatAt)
	if !ok {
		return th.MinutesUntilClose(now)
	}
	return ff - th.nowMinutes(now)
}

// NthTradingDayFrom は now から n 営業日先の日付(venue TZ の 0 時)を返す。
// n=0 は当日。土日と休場日は数えない。
//
// 用途は broker 側の守り注文の期日(立花 sOrderExpireDay は「0=当日」か YYYYMMDD)。
// 当日期限のままだと多日保有の建玉は**2日目に守りが消える**。
//
// 🛑 カレンダーが尽きたら zero を返す(fail-close)。「たぶん営業日だろう」で期日を
// 置くと、休場日を指定して注文ごと拒否されるか、意図より短い期限で黙って失効する。
// zero を受けた側は当日期限に落とすなり拒否するなりを**明示的に**決める。
func (th TradingHours) NthTradingDayFrom(now time.Time, n int) time.Time {
	day := th.DayStart(now)
	for i := 0; ; {
		if !th.CalendarCovers(day) {
			return time.Time{}
		}
		if th.IsTradingDay(day) {
			if i == n {
				return day
			}
			i++
		}
		day = day.AddDate(0, 0, 1)
	}
}
