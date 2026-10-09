package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/session"
)

func acHours() session.TradingHours {
	return session.TradingHours{
		TZ: clock.JST, ForceFlatAt: "14:50",
		Sessions:        []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:30"}},
		Holidays:        map[string]struct{}{"2026-08-11": {}},
		CalendarThrough: time.Date(2026, 12, 31, 0, 0, 0, 0, clock.JST),
	}
}

func at(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, clock.JST)
}

// 引け前は走らない / 引け後は 1 営業日 1 回だけ走る。
func TestAfterClose_RunsOncePerTradingDay(t *testing.T) {
	now := at(2026, 8, 14, 15, 0)
	ac := NewAfterClose("15:40", acHours(), func() time.Time { return now }, nil)
	runs := 0
	ac.Add("j", func(context.Context) error { runs++; return nil })

	ac.Tick(context.Background())
	if runs != 0 {
		t.Fatalf("引け前に走った: %d", runs)
	}
	now = at(2026, 8, 14, 15, 40)
	ac.Tick(context.Background())
	ac.Tick(context.Background())
	if runs != 1 {
		t.Fatalf("runs = %d, want 1(1 営業日 1 回)", runs)
	}
	now = at(2026, 8, 17, 16, 0) // 翌営業日はまた走る
	ac.Tick(context.Background())
	if runs != 2 {
		t.Fatalf("翌営業日に走っていない: %d", runs)
	}
}

// 🛑 休場日は走らせない(空の記録を作らない)。
func TestAfterClose_SkipsHolidays(t *testing.T) {
	now := at(2026, 8, 11, 16, 0) // 山の日
	ac := NewAfterClose("15:40", acHours(), func() time.Time { return now }, nil)
	runs := 0
	ac.Add("j", func(context.Context) error { runs++; return nil })
	ac.Tick(context.Background())
	ac.RunNow(context.Background())
	if runs != 0 {
		t.Fatalf("休場日に走った: %d", runs)
	}
}

// 🛑 停止時は**時刻を問わず**走らせる。引け直後に停止されると Tick が一度も条件を
// 満たさず、当日の記録(段1)が書かれない。
func TestAfterClose_RunNowCatchesEarlyStop(t *testing.T) {
	now := at(2026, 8, 14, 15, 31)
	ac := NewAfterClose("15:40", acHours(), func() time.Time { return now }, nil)
	runs := 0
	ac.Add("j", func(context.Context) error { runs++; return nil })
	ac.RunNow(context.Background())
	if runs != 1 {
		t.Fatalf("停止時に走っていない: %d", runs)
	}
	ac.RunNow(context.Background()) // 同日に二重実行しない
	if runs != 1 {
		t.Fatalf("同日に二重実行した: %d", runs)
	}
}

// 🛑 記録の失敗・panic で bot を落とさない。次の営業日にまた試す。
func TestAfterClose_SurvivesFailureAndPanic(t *testing.T) {
	now := at(2026, 8, 14, 16, 0)
	ac := NewAfterClose("15:40", acHours(), func() time.Time { return now }, nil)
	ac.Add("boom", func(context.Context) error { panic("boom") })
	ac.Add("err", func(context.Context) error { return errors.New("x") })
	ran := false
	ac.Add("ok", func(context.Context) error { ran = true; return nil })
	ac.Tick(context.Background())
	if !ran {
		t.Fatal("先行ジョブの失敗で後続が走らなくなっている")
	}
}

// 🛑 遅いジョブ(LLM を呼ぶ段2)は**停止経路では走らせない**。`make stop` が分単位で
// ブロックされると、人間は Ctrl-C で殺すことを覚え、graceful shutdown 自体が壊れる。
// 取りこぼしは翌営業日の scheduled 実行が backfill で拾う。
func TestAfterClose_SlowJobsSkipTheStopPath(t *testing.T) {
	now := at(2026, 8, 14, 15, 31)
	ac := NewAfterClose("15:40", acHours(), func() time.Time { return now }, nil)
	fast, slow := 0, 0
	ac.Add("fast", func(context.Context) error { fast++; return nil })
	ac.AddSlow("slow", time.Minute, func(context.Context) error { slow++; return nil })

	ac.RunNow(context.Background())
	if fast != 1 || slow != 0 {
		t.Fatalf("fast=%d slow=%d — 停止時に走ってよいのは fast だけ", fast, slow)
	}
	// 停止経路で skip した日は「その日は済んだ」印を付けない。同日中に RunAt へ
	// 到達すれば走る(印を付けてしまうと当日ぶんが永久に落ちる)。
	now = at(2026, 8, 14, 15, 45)
	ac.Tick(context.Background())
	if slow != 1 {
		t.Fatalf("slow=%d — skip した日に scheduled で走っていない", slow)
	}
}

// ジョブごとのタイムアウト。段2 は LLM を複数日ぶん回すので既定より長い枠が要る。
func TestAfterClose_PerJobTimeout(t *testing.T) {
	now := at(2026, 8, 14, 16, 0)
	ac := NewAfterClose("15:40", acHours(), func() time.Time { return now }, nil)
	ac.Timeout = time.Hour
	var got time.Duration
	ac.AddSlow("slow", 42*time.Minute, func(ctx context.Context) error {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("deadline が無い")
		}
		got = time.Until(dl).Round(time.Minute)
		return nil
	})
	ac.Tick(context.Background())
	if got != 42*time.Minute {
		t.Fatalf("per-job timeout = %v, want 42m", got)
	}
}

func TestAfterMinutes(t *testing.T) {
	if afterMinutes(at(2026, 8, 14, 15, 39), "15:40") {
		t.Fatal("15:39 は 15:40 前")
	}
	if !afterMinutes(at(2026, 8, 14, 15, 40), "15:40") {
		t.Fatal("15:40 は含む")
	}
	if afterMinutes(at(2026, 8, 14, 23, 0), "xx") {
		t.Fatal("形式不正で走らせない")
	}
}

// 朝枠: 前日の日次総評(段2)を寄り前に書く。段2 は前日の日足が要り、それは
// 翌朝の日足取得でしか入らないので、これまでは翌日 15:40 まで丸 1 日遅れていた。
// 🛑 時間帯の内側で ready のときだけ・1 営業日 1 回。ready でない間は印を付けない
// (日足取得が終わるまで毎分待つ。印を付けると当日の朝枠が永久に落ちる)。
func TestAfterClose_MorningJobRunsInWindowOnceReady(t *testing.T) {
	now := at(2026, 8, 14, 7, 30)
	ready := false
	ac := NewAfterClose("15:40", acHours(), func() time.Time { return now }, nil)
	runs := 0
	ac.AddMorning("m", "07:00", "08:30", time.Minute, func(time.Time) bool { return ready },
		func(context.Context) error { runs++; return nil })

	ac.Tick(context.Background())
	if runs != 0 {
		t.Fatal("ready でないのに走った(日足取得の途中で総評を固定する)")
	}
	ready = true
	now = at(2026, 8, 14, 7, 45)
	ac.Tick(context.Background())
	ac.Tick(context.Background())
	if runs != 1 {
		t.Fatalf("runs = %d, want 1(朝枠は 1 営業日 1 回)", runs)
	}
	now = at(2026, 8, 14, 15, 40) // 引け後の枠で二重に走らない
	ac.Tick(context.Background())
	if runs != 1 {
		t.Fatalf("15:40 に朝枠ジョブがもう一度走った: %d", runs)
	}
	now = at(2026, 8, 17, 7, 10) // 翌営業日はまた走る
	ac.Tick(context.Background())
	if runs != 2 {
		t.Fatalf("翌営業日の朝に走っていない: %d", runs)
	}
}

// 🛑 時間帯の外では走らせない。08:30 より後に始めると寄り付き(advisor の最初の判断は 08:45 頃)
// と Claude の枠を取り合う。間に合わない日は従来どおり 15:40 の枠が書く。
func TestAfterClose_MorningJobStaysInsideWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		now  time.Time
	}{
		{"開始前", at(2026, 8, 14, 6, 59)},
		{"締切後", at(2026, 8, 14, 8, 30)},
		{"引け後", at(2026, 8, 14, 16, 0)},
		{"休場日", at(2026, 8, 11, 7, 30)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := tc.now
			ac := NewAfterClose("15:40", acHours(), func() time.Time { return now }, nil)
			runs := 0
			ac.AddMorning("m", "07:00", "08:30", time.Minute, func(time.Time) bool { return true },
				func(context.Context) error { runs++; return nil })
			ac.Tick(context.Background())
			if runs != 0 {
				t.Fatalf("時間帯の外(%v)で走った", now)
			}
		})
	}
}

// 🛑 朝枠は停止経路で走らせない(段2 は LLM で分単位 — 停止後もプロセスが居残る)。
func TestAfterClose_MorningJobSkipsTheStopPath(t *testing.T) {
	now := at(2026, 8, 14, 7, 30)
	ac := NewAfterClose("15:40", acHours(), func() time.Time { return now }, nil)
	runs := 0
	ac.AddMorning("m", "07:00", "08:30", time.Minute, func(time.Time) bool { return true },
		func(context.Context) error { runs++; return nil })
	ac.RunNow(context.Background())
	if runs != 0 {
		t.Fatal("停止経路で朝枠ジョブが走った")
	}
	ac.Tick(context.Background()) // 停止経路で skip しても同日の朝枠は残る
	if runs != 1 {
		t.Fatalf("runs = %d, want 1", runs)
	}
}
