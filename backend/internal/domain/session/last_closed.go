package session

import "time"

// 前営業日探索の上限。東証がこれほど連続休場することは無いので、到達 = カレンダーが壊れている合図。
const maxTradingDayLookback = 14

// 既に引けた最新セッションの venue 日付(= 存在しうる最新の日足)。「今日の足はまだ要るか」に答える —
// これが無いと日足リフレッシュが既に持つ足を broker へ引き直す(立花から高負荷と指摘された経路)。
// ok=false は「分からない」(休場カレンダー期限切れ / セッション未設定)。呼び手は取得側へ倒すこと
// — 安全な方向は呼び出し節約ではなく古いデータの回避。
func (th TradingHours) LastClosedTradingDay(now time.Time) (string, bool) {
	tz := th.TZ
	if tz == nil {
		tz = time.UTC
	}
	closeAt, ok := th.closeMinute()
	if !ok {
		return "", false
	}
	t := now.In(tz)
	// 今日が数えられるのは最終セッションが引けてから。
	if !(th.IsTradingDay(now) && th.nowMinutes(now) >= closeAt) {
		t = t.AddDate(0, 0, -1)
	}
	for i := 0; i < maxTradingDayLookback; i++ {
		if !th.CalendarCovers(t) {
			return "", false // stale calendar → refuse to guess
		}
		if th.IsTradingDay(t) {
			return t.Format("2006-01-02"), true
		}
		t = t.AddDate(0, 0, -1)
	}
	return "", false
}

func (th TradingHours) closeMinute() (int, bool) {
	last, found := 0, false
	for _, w := range th.Sessions {
		e, ok := minutesOfDay(w.End)
		if !ok {
			continue
		}
		if !found || e > last {
			last, found = e, true
		}
	}
	return last, found
}
