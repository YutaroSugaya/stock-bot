package main

import (
	"time"

	"stockbot/backend/internal/domain/session"
)

// csvMaxLookbackDays は「直前の営業日」を探す遡り上限。年末年始でも 10 日で足りる。
const csvMaxLookbackDays = 10

// csvFreshThrough returns the newest 日足 the CSV can legitimately hold right now.
//
// DB 側の基準(LastClosedTradingDay)とは**わざと1営業日ずらす**。fetch-daily は
// 当日(JST)のバーを取り込まない — 立花の日足は夜間バッチまで未確定でありうる一方、
// CSV のマージは append-only で一度入った未確定足は自己修復しないため。
//
// この差を織り込まないと引け後から翌朝の取得までの間ずっと全銘柄が「遅れている」と
// 鳴り、常時鳴る警告が本物の停止を隠す(= この警告を足した目的が無力化される)。
//
// 空文字 = 判定不能(休場カレンダー失効)。呼び出し側は鮮度判定をしない。
func csvFreshThrough(hours session.TradingHours, now time.Time) string {
	d, ok := hours.LastClosedTradingDay(now)
	if !ok {
		return ""
	}
	tz := hours.TZ
	if tz == nil {
		tz = time.UTC
	}
	local := now.In(tz)
	if d == local.Format("2006-01-02") {
		// 引け後: 当日足はまだ CSV に入らないので、直前の営業日まで戻す。
		d = prevTradingDay(hours, local)
	}
	// 🛑 **日付が変わっただけでは足は増えない。** 前営業日ぶんを CSV に入れるのは
	// 朝ジョブ(07:00 の fetch-daily)で、それが終わるまでその足はどこにも存在しない。
	// 00:00 に基準日だけ進めると、bot を上げっぱなしにした晩は毎回 0時〜8時のあいだ
	// 全銘柄が「遅れている」と鳴る。**常時鳴る警告は本物の停止を
	// 隠す** — この監視を足した目的そのものが無力化される。
	if d != "" && local.Hour() < csvMorningFetchDoneHourJST {
		day, err := time.ParseInLocation("2006-01-02", d, tz)
		if err != nil {
			return ""
		}
		d = prevTradingDay(hours, day)
	}
	return d
}

// csvMorningFetchDoneHourJST は朝ジョブ(07:00 起動・プール取得に最大40分)が
// 終わっているとみなす時刻。これ以降なら「入っていない = 本物の失敗」と鳴らしてよい。
const csvMorningFetchDoneHourJST = 8

func prevTradingDay(hours session.TradingHours, from time.Time) string {
	d, ok := hours.PrevTradingDay(from, csvMaxLookbackDays)
	if !ok {
		return ""
	}
	return d.Format("2006-01-02")
}
