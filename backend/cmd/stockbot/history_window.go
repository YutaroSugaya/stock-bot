package main

import (
	"stockbot/backend/internal/domain/clock"
	"time"
)

// history_window.go encodes 立花's published guidance on WHEN to pull history.
//
// 出典 https://www.e-shiten.jp/api/20260310.html: 履歴・マスタの
// 取得は **PM18:00〜翌3:30 / AM5:30〜AM8:00** を推奨、**AM8:00〜PM15:30** は大量かつ
// 頻繁な株価取得とポーリングを控える。回数の上限は非公表なので守れるのは時間帯と頻度だけ。
//
// 窓の終端 3:30 は API 用サーバの閉局時刻と一致する。閉局を跨いで引き続けると
// セッション無効の再ログインが銘柄ごとに走る。
const (
	historyEveningStart = 18*60 + 0 // PM18:00
	historyEveningEnd   = 3*60 + 30 // 翌AM3:30 (閉局)
	historyMorningStart = 5*60 + 30 // AM5:30
	historyMorningEnd   = 8*60 + 0  // AM8:00
)

// inHistoryFetchWindow reports whether now falls in a window 立花 recommends for
// history/master downloads. The startup seed is deliberately exempt.
func inHistoryFetchWindow(now time.Time) bool {
	t := now.In(clock.JST)
	m := t.Hour()*60 + t.Minute()
	// 夜間窓は日を跨ぐので OR で判定する。
	if m >= historyEveningStart || m < historyEveningEnd {
		return true
	}
	return m >= historyMorningStart && m < historyMorningEnd
}
