package broker

import (
	"context"
	"sync"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
)

// fakeSleeper advances a movable clock instead of really sleeping, so the
// limiter's pacing is asserted deterministically.
type fakeSleeper struct {
	mu     sync.Mutex
	now    time.Time
	slept  []time.Duration
	cancel bool
}

func newFakeSleeper(start time.Time) *fakeSleeper { return &fakeSleeper{now: start} }

func (f *fakeSleeper) clock() clock.Clock {
	return func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
}

func (f *fakeSleeper) sleep(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancel {
		return context.Canceled
	}
	f.slept = append(f.slept, d)
	f.now = f.now.Add(d)
	return nil
}

func (f *fakeSleeper) waits() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.slept...)
}

func testLimiter(rps float64, burst int, f *fakeSleeper) *rateLimiter {
	l := newRateLimiter(rps, burst, f.clock())
	l.sleepFn = f.sleep
	return l
}

func TestRateLimiter_BurstPassesWithoutWaiting(t *testing.T) {
	f := newFakeSleeper(time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC))
	l := testLimiter(2, 5, f)

	for i := 0; i < 5; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("burst call %d: %v", i, err)
		}
	}
	if got := f.waits(); len(got) != 0 {
		t.Fatalf("burst must not sleep, slept %v", got)
	}
}

// The whole point of the limiter: past the burst, calls are spaced at 1/rps no
// matter how many goroutines want to fire (立花 から高負荷と指摘された原因)。
func TestRateLimiter_PacesPastBurst(t *testing.T) {
	f := newFakeSleeper(time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC))
	l := testLimiter(2, 1, f) // 2 rps = 500ms spacing

	for i := 0; i < 4; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	got := f.waits()
	if len(got) != 3 { // first call consumes the single burst token
		t.Fatalf("want 3 paced waits, got %v", got)
	}
	for i, d := range got {
		if d != 500*time.Millisecond {
			t.Fatalf("wait %d = %v, want 500ms", i, d)
		}
	}
}

// Idle time must refill the bucket, else a bot that polls slowly would still be
// throttled after a quiet period.
func TestRateLimiter_RefillsWhileIdle(t *testing.T) {
	f := newFakeSleeper(time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC))
	l := testLimiter(2, 2, f)

	for i := 0; i < 2; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("drain %d: %v", i, err)
		}
	}
	f.mu.Lock()
	f.now = f.now.Add(10 * time.Second) // idle → bucket refills to burst
	f.mu.Unlock()

	for i := 0; i < 2; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("after idle %d: %v", i, err)
		}
	}
	if got := f.waits(); len(got) != 0 {
		t.Fatalf("refilled bucket must not sleep, slept %v", got)
	}
}

func TestRateLimiter_HonoursContextCancel(t *testing.T) {
	f := newFakeSleeper(time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC))
	l := testLimiter(2, 1, f)

	if err := l.Wait(context.Background()); err != nil { // drains the burst token
		t.Fatalf("burst call: %v", err)
	}
	f.mu.Lock()
	f.cancel = true // the next call must wait — and that wait is cancelled
	f.mu.Unlock()

	if err := l.Wait(context.Background()); err == nil {
		t.Fatal("a cancelled sleep must surface as an error, not a silent send")
	}
}

// A nil limiter is the "unlimited" wiring (paper / tests) — it must not panic.
func TestRateLimiter_NilIsUnlimited(t *testing.T) {
	var l *rateLimiter
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("nil limiter must pass through: %v", err)
	}
}

// rps <= 0 disables limiting entirely (explicit opt-out via env).
func TestNewRateLimiter_NonPositiveRPSIsUnlimited(t *testing.T) {
	if l := newRateLimiter(0, 5, nil); l != nil {
		t.Fatal("rps=0 must yield no limiter (unlimited)")
	}
}
