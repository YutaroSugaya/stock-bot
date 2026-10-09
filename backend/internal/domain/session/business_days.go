package session

import "time"

// AddBusinessDays は t から n **営業日**後の同時刻を返す(n<=0 は t をそのまま返す)。
// 戦略別 MaxHold(入口の窓 × 0.5 営業日)を分に直すための唯一の換算器。
//
// 🛑 IsTradingDay を使わない。あちらは CalendarThrough を過ぎると**全ての日を false**
// に倒す fail-close で、126 営業日先を要求する abs_momentum_v2 では「取引日が永久に
// 見つからない」= 無限ループになる。ここが見るのは「週末か / 既知の休場日か」だけ。
//
// ⚠ したがって **休場カレンダーの horizon の外では週末しか飛ばせない**。63〜126 営業日の
// ような長い MaxHold の期限は、未登録の祝日ぶんだけ実際より手前に落ちうる。max_hold は
// 「その時刻を過ぎた最初のティックで閉じる」ソフト期限なので害は無い(期限が休場日に
// 落ちれば翌営業日に閉じるだけ)。**新規建てや守りの期日には使わない。**
//
// ⚠ BNF 家族も営業日(引け前に揃えるのは MaxHoldDeadline)。短い値ほど
// 暦日/営業日の差が効くため。
func (th TradingHours) AddBusinessDays(t time.Time, n int) time.Time {
	if n <= 0 {
		return t
	}
	tz := th.TZ
	if tz == nil {
		tz = time.UTC
	}
	cur := t
	for left := n; left > 0; {
		cur = cur.AddDate(0, 0, 1)
		if th.isBusinessDay(cur.In(tz)) {
			left--
		}
	}
	return cur
}

// 週末でも既知の休場日でもない日。CalendarThrough は見ない(AddBusinessDays の注記)。
func (th TradingHours) isBusinessDay(t time.Time) bool {
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

// BusinessDaysBetween は from から to までの**営業日数**(from 当日は数えない)。
// to <= from は 0。AddBusinessDays と同じく休場カレンダーの horizon 外では週末だけを
// 飛ばす(祝日を知らないぶん実際より多めに出る)。
//
// 🛑 旧台帳を閉じる判断材料(残玉の経過営業日)がこれを使う。
// 暦日で数えると週末を跨ぐたび 2 日ぶん水増しされ、「何営業日 開いているか」という
// 問いに別の数字で答えることになる。
func (th TradingHours) BusinessDaysBetween(from, to time.Time) int {
	if !to.After(from) {
		return 0
	}
	tz := th.TZ
	if tz == nil {
		tz = time.UTC
	}
	n := 0
	cur := from
	for {
		cur = cur.AddDate(0, 0, 1)
		if cur.After(to) {
			return n
		}
		if th.isBusinessDay(cur.In(tz)) {
			n++
		}
	}
}

// MaxHoldDeadline は多日建玉の時間切れの期限を返す: t から n 営業日後の日の **ForceFlatAt
// (引け前 14:50)**。損益に依らずこの時刻で決済する。
//
// 同時刻(AddBusinessDays)のままだと、期限が建てた時刻に引きずられて寄りの荒い値で
// 成行になる(寄り直後の 09:02 に決済された例がある)。
//
// Sessions / ForceFlatAt が読めない呼び手(backtest の日足リプレイ)は ok=false で
// **従来の「n 営業日後の同時刻」**を返す — 無期限には縮退させない。n<=0 は t をそのまま返す。
func (th TradingHours) MaxHoldDeadline(t time.Time, n int) (deadline time.Time, ok bool) {
	if n <= 0 {
		return t, false
	}
	same := th.AddBusinessDays(t, n)
	ff, okFF := minutesOfDay(th.ForceFlatAt)
	if len(th.Sessions) == 0 || !okFF {
		return same, false
	}
	d := same.In(th.zone())
	return time.Date(d.Year(), d.Month(), d.Day(), ff/60, ff%60, 0, 0, th.zone()), true
}

// CloseAlignedDeadlines は after より後・through 以前にある**各営業日の ForceFlatAt**
// を昇順に返す。max_hold 延長の候補(画面のセレクト)を作るためのもので、延長先を
// MaxHoldDeadline と同じ「引け前」に揃える — 任意の時刻へ延ばすと期限が寄りや
// 夜間に落ち、次の立会の最初の値で成行になる。
//
// 営業日の判定は AddBusinessDays と同じ(週末 + 既知の休場日。horizon の外は週末だけ)。
// horizon の外かどうかは呼び手が CalendarCovers で印を付ける。
// Sessions / ForceFlatAt が読めない呼び手には何も返さない(時刻を捏造しない)。
func (th TradingHours) CloseAlignedDeadlines(after, through time.Time) []time.Time {
	ff, ok := minutesOfDay(th.ForceFlatAt)
	if len(th.Sessions) == 0 || !ok {
		return nil
	}
	tz := th.zone()
	var out []time.Time
	d := after.In(tz)
	for day := time.Date(d.Year(), d.Month(), d.Day(), ff/60, ff%60, 0, 0, tz); !day.After(through); day = day.AddDate(0, 0, 1) {
		if day.After(after) && th.isBusinessDay(day) {
			out = append(out, day)
		}
	}
	return out
}
