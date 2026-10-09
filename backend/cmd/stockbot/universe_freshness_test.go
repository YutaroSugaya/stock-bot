package main

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/session"
)

func ufHours() session.TradingHours {
	return session.TradingHours{
		TZ: clock.JST, ForceFlatAt: "14:50",
		Sessions:        []session.Window{{Start: "09:00", End: "15:30"}},
		Holidays:        map[string]struct{}{"2026-08-11": {}},
		CalendarThrough: time.Date(2026, 12, 31, 0, 0, 0, 0, clock.JST),
	}
}

func jst(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, clock.JST)
}

// 🛑 bot はユニバースを**起動時にしか読まない**。起動しっぱなしにすると
// 「毎朝選び直す200銘柄」という事前登録が黙って破れる。取引日にそれを検出する。
func TestStaleUniverse(t *testing.T) {
	h := ufHours()
	cases := []struct {
		name        string
		loaded, now time.Time
		wantDays    int
		wantWarn    bool
	}{
		{"当日のユニバースなら黙る", jst(2026, 8, 14, 7, 12), jst(2026, 8, 14, 15, 0), 0, false},
		{"前営業日のまま = 警告", jst(2026, 8, 13, 7, 13), jst(2026, 8, 14, 9, 5), 1, true},
		{"連休を跨いで古い", jst(2026, 8, 10, 7, 20), jst(2026, 8, 14, 9, 5), 4, true},
		// 休場日は選び直されないので、古くて当然。警告しない(狼少年にしない)。
		{"休場日は警告しない", jst(2026, 8, 10, 7, 20), jst(2026, 8, 11, 9, 5), 1, false},
		{"土曜も警告しない", jst(2026, 8, 14, 7, 12), jst(2026, 8, 15, 9, 5), 1, false},
		// 07:00 の選定より前に起動した取引日: その日ぶんはまだ無いので警告しない。
		// (make start の catchup が起動前に選び直すが、素の run では起きうる)
		{"寄り前の早朝は警告しない", jst(2026, 8, 13, 7, 13), jst(2026, 8, 14, 6, 30), 1, false},
		{"mtime 不明なら黙る", time.Time{}, jst(2026, 8, 14, 9, 5), 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			days, warn := staleUniverse(c.loaded, c.now, h)
			if warn != c.wantWarn {
				t.Fatalf("warn = %v, want %v", warn, c.wantWarn)
			}
			if warn && days != c.wantDays {
				t.Fatalf("days = %d, want %d", days, c.wantDays)
			}
		})
	}
}
