package main

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
)

// 板の写しの更新窓を `InTradingHours || 寄り前` で書いたところ、**11:30〜12:30 が
// どちらにも入らない**。11:49 に再起動したら板を一度も読めず、画面は台帳の凍結値の
// まま 12:30 まで固まった。「取引時間の和集合」で日中を表そうとすると、
// **場の切れ目が必ず落ちる**。素直に時刻の範囲で書く。
func TestProtectiveBoardWindowCoversTheLunchBreak(t *testing.T) {
	h := replaceLoopHours()
	at := func(m, d, hh, mm int) time.Time {
		return time.Date(2026, time.Month(m), d, hh, mm, 0, 0, clock.JST)
	}
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"寄り前 07:30", at(8, 25, 7, 30), true},
		{"前場 10:00", at(8, 25, 10, 0), true},
		{"昼休み 11:49 — 実機で落ちた時刻", at(8, 25, 11, 49), true},
		{"昼休み 12:00", at(8, 25, 12, 0), true},
		{"後場 13:00", at(8, 25, 13, 0), true},
		{"引け直後 15:45", at(8, 25, 15, 45), true},
		// 🛑 深夜は回さない。建玉の数だけ照会が飛ぶうえ、broker のメンテ窓に当たる。
		{"深夜 03:00", at(8, 25, 3, 0), false},
		{"早朝 06:00", at(8, 25, 6, 0), false},
		{"夜 20:00", at(8, 25, 20, 0), false},
		{"休場日(日曜)", at(8, 23, 10, 0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldRefreshProtectiveBoard(h, c.now); got != c.want {
				t.Fatalf("shouldRefreshProtectiveBoard(%s) = %v, want %v",
					c.now.Format("01-02 15:04"), got, c.want)
			}
		})
	}
}
