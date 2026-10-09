package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/session"
)

// AfterClose は**引け後に 1 営業日 1 回だけ走る記録ジョブ**の入れ物。
//
// なぜ bot の中でやるか: 人間の運用は `make start` / `make stop` だけ、という運用方針。
// launchd への登録は Makefile を触るので人間の作業になってしまう。bot は
// start〜stop の間ずっと生きているので、そこに載せれば追加の運用がゼロになる。
//
// 🛑 **取引経路から完全に切り離す**。ジョブは
//   - 別 goroutine で走り、価格ループ・注文経路とは何も共有しない
//   - panic を握り潰す(記録の失敗で bot を落とさない)
//   - タイムアウトを持つ(外部 HTTP と LLM を呼ぶので、掴んだまま生き残らせない)
//   - 失敗しても次の営業日にまた試す(その日の記録は諦める)
//
// 🛑 **停止時にも 1 回走らせる**。引け(15:30)直後に `make stop` されると `RunAt` に
// 到達しないため(当日の段1 が書かれない)。二重実行は無害(同日は 1 回だけ)。
type AfterClose struct {
	// RunAt は JST の "HH:MM"。大引け 15:30 の後に置く。
	RunAt   string
	Hours   session.TradingHours
	Clock   clock.Clock
	Logger  *slog.Logger
	Timeout time.Duration

	jobs []afterCloseJob

	mu     sync.Mutex
	lastAt map[string]string // job 名 → 最後に走った JST 日
}

const triggerOnStop = "on-stop"

type afterCloseJob struct {
	name string
	run  func(context.Context) error
	// slow なジョブは**停止経路では走らせない**。
	//
	// 🛑 理由は「`make stop` がブロックされるから」**ではない**:
	// HTTP サーバの Shutdown は RunNow より先に走ってポートが空くので、`make stop` の
	// 待ちループはすぐ抜ける。起きるのは逆で、**「停止しました」と表示された後も
	// プロセスが分単位で居残る** — その間に `make start` を打つと 2 プロセスが並走する。
	// 取りこぼしは翌営業日の backfill に任せるほうが安全。
	slow bool
	// timeout は 0 なら AfterClose.Timeout。
	timeout time.Duration
	// morning は朝枠(AddMorning)。RunAt ではなく自分の時間帯で走り、停止経路では走らない。
	morning *morningWindow
}

// morningWindow は朝枠ジョブの時間帯 [from, until) と前提条件。
type morningWindow struct {
	from, until string
	ready       func(time.Time) bool
}

func NewAfterClose(runAt string, hours session.TradingHours, c clock.Clock, logger *slog.Logger) *AfterClose {
	if c == nil {
		c = clock.System()
	}
	return &AfterClose{RunAt: runAt, Hours: hours, Clock: c, Logger: logger,
		Timeout: 15 * time.Minute, lastAt: map[string]string{}}
}

// Add registers a job. 名前は「1 営業日 1 回」の単位。
func (a *AfterClose) Add(name string, run func(context.Context) error) {
	a.jobs = append(a.jobs, afterCloseJob{name: name, run: run})
}

// AddSlow registers a job that は停止経路を飛ばす(上の `slow` を参照)。timeout は
// そのジョブ専用の上限(0 なら既定)。
func (a *AfterClose) AddSlow(name string, timeout time.Duration, run func(context.Context) error) {
	a.jobs = append(a.jobs, afterCloseJob{name: name, run: run, slow: true, timeout: timeout})
}

// AddMorning registers a job that 朝の時間帯 [from, until)(JST "HH:MM")に、ready が真に
// なった最初の Tick で 1 営業日 1 回だけ走る。ready が偽の間は印を付けない(条件が整う
// まで毎 Tick 待つ)。RunAt の枠と停止経路では走らない。
//
// 用途は前日の日次総評(段2)を寄り前に書くこと。段2 は前日の日足が要り、
// それは翌朝の日足取得でしか入らないので、引け後の枠だけだと丸 1 日遅れていた。
func (a *AfterClose) AddMorning(name, from, until string, timeout time.Duration, ready func(time.Time) bool, run func(context.Context) error) {
	a.jobs = append(a.jobs, afterCloseJob{name: name, run: run, timeout: timeout,
		morning: &morningWindow{from: from, until: until, ready: ready}})
}

// JobNames / SlowJobCount は**配線を固定するため**の introspection。呼び手側
// (`cmd/stockbot`)のテストが「段2 が live で登録されていないこと」「AddSlow で
// 登録されていること」を検査できないと、戻されても全部緑のまま通る。
func (a *AfterClose) JobNames() []string {
	out := make([]string, 0, len(a.jobs))
	for _, j := range a.jobs {
		out = append(out, j.name)
	}
	return out
}

func (a *AfterClose) SlowJobCount() int {
	n := 0
	for _, j := range a.jobs {
		if j.slow {
			n++
		}
	}
	return n
}

// Tick は定期的に呼ばれる。**取引日の RunAt 以降**で、その日まだ走っていないジョブを走らせる。
func (a *AfterClose) Tick(ctx context.Context) {
	now := a.Clock()
	if !a.Hours.IsTradingDay(now) {
		return // 休場日は記録しない(空の記録を作らない)
	}
	a.runMorning(ctx, now)
	if !afterMinutes(now, a.RunAt) {
		return
	}
	a.runDue(ctx, now, "scheduled")
}

// runMorning は時間帯の内側で ready になった朝枠ジョブを走らせる。
func (a *AfterClose) runMorning(ctx context.Context, now time.Time) {
	day := now.In(clock.JST).Format("2006-01-02")
	for _, job := range a.jobs {
		w := job.morning
		if w == nil || !afterMinutes(now, w.from) || afterMinutes(now, w.until) {
			continue
		}
		a.mu.Lock()
		done := a.lastAt[job.name] == day
		a.mu.Unlock()
		if done || (w.ready != nil && !w.ready(now)) {
			continue // ready でない間は印を付けない(条件が整うまで待つ)
		}
		a.mu.Lock()
		a.lastAt[job.name] = day
		a.mu.Unlock()
		a.runOne(ctx, job, "morning")
	}
}

// RunNow は停止時の取りこぼし回収。**取引日なら時刻を問わず**走らせる — 引け直後に
// stop されると Tick が一度も条件を満たさないため。
func (a *AfterClose) RunNow(ctx context.Context) {
	now := a.Clock()
	if !a.Hours.IsTradingDay(now) {
		return
	}
	a.runDue(ctx, now, triggerOnStop)
}

func (a *AfterClose) runDue(ctx context.Context, now time.Time, trigger string) {
	day := now.In(clock.JST).Format("2006-01-02")
	for _, job := range a.jobs {
		if job.morning != nil {
			continue // 朝枠は runMorning だけが走らせる(引け後の枠・停止経路では走らない)
		}
		if job.slow && trigger == triggerOnStop {
			// 印を付けずに飛ばす。同日中に RunAt へ到達すれば scheduled で走る。
			continue
		}
		a.mu.Lock()
		done := a.lastAt[job.name] == day
		if !done {
			a.lastAt[job.name] = day // 先に印を付ける(失敗しても同日に連打しない)
		}
		a.mu.Unlock()
		if done {
			continue
		}
		a.runOne(ctx, job, trigger)
	}
}

func (a *AfterClose) runOne(ctx context.Context, job afterCloseJob, trigger string) {
	defer func() {
		// 記録の失敗で bot を落とさない。取引経路とは何も共有していない。
		if r := recover(); r != nil && a.Logger != nil {
			a.Logger.Error("after-close job panicked", "job", job.name, "panic", r)
		}
	}()
	timeout := job.timeout
	if timeout <= 0 {
		timeout = a.Timeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := a.Clock()
	if err := job.run(cctx); err != nil {
		if a.Logger != nil {
			a.Logger.Warn("after-close job failed", "job", job.name, "trigger", trigger, "err", err)
		}
		return
	}
	if a.Logger != nil {
		a.Logger.Info("after-close job done", "job", job.name, "trigger", trigger,
			"took", a.Clock().Sub(started).Round(time.Second).String())
	}
}

// afterMinutes は now(JST)が "HH:MM" 以降か。形式不正は false(勝手に走らせない)。
func afterMinutes(now time.Time, hhmm string) bool {
	var h, m int
	if n, err := fmt.Sscanf(hhmm, "%d:%d", &h, &m); err != nil || n != 2 {
		return false
	}
	j := now.In(clock.JST)
	return j.Hour()*60+j.Minute() >= h*60+m
}
