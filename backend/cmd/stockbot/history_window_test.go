package main

import (
	"stockbot/backend/internal/domain/clock"
	"testing"
	"time"
)

// 立花の公式アナウンス(https://www.e-shiten.jp/api/20260310.html): 履歴・マスタの
// 取得は PM18:00〜翌3:30 / AM5:30〜AM8:00 を推奨、AM8:00〜PM15:30 は控える。
func TestInHistoryFetchWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 7, 29, h, m, 0, 0, clock.JST) }

	cases := []struct {
		h, m int
		want bool
		why  string
	}{
		{18, 0, true, "夜間窓の開始"},
		{20, 0, true, "夜間窓"},
		{23, 59, true, "日跨ぎ前"},
		{0, 30, true, "日跨ぎ後も夜間窓"},
		{3, 29, true, "夜間窓の終端直前"},
		{3, 30, false, "API 閉局 = 窓の外"},
		{4, 0, false, "閉局中"},
		{5, 29, false, "朝窓の直前"},
		{5, 30, true, "朝窓の開始"},
		{7, 30, true, "朝窓"},
		{8, 0, false, "AM8:00 以降は控える時間帯"},
		{9, 0, false, "場中"},
		{12, 0, false, "昼休みでも控える時間帯"},
		{15, 29, false, "大引け前"},
		{15, 30, false, "PM15:30 ちょうどはまだ窓外"},
		{17, 59, false, "夜間窓の直前"},
	}
	for _, c := range cases {
		if got := inHistoryFetchWindow(at(c.h, c.m)); got != c.want {
			t.Errorf("%02d:%02d (%s) = %v, want %v", c.h, c.m, c.why, got, c.want)
		}
	}
}
