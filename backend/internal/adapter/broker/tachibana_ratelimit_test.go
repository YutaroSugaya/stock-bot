package broker

import (
	"context"
	"testing"
	"time"
)

// Every session-bound CLM must pass through the limiter. 立花's high-load
// complaint happened because nothing bounded the call rate; a
// limiter that some code path can bypass is not a ceiling at all.
func TestTachibanaRequestGoesThroughRateLimiter(t *testing.T) {
	tb, _, ts := newMockTachibana(t, false, defaultRespond)
	defer ts.Close()

	f := newFakeSleeper(time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC))
	tb.limiter = testLimiter(2, 1, f) // 500ms spacing, one burst token

	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := tb.GetTicker(ctx, "7203"); err != nil {
			t.Fatalf("ticker %d: %v", i, err)
		}
	}
	if got := f.waits(); len(got) == 0 {
		t.Fatal("quote polling must be paced by the limiter, but nothing waited")
	}
}

// login は requestOnce を通らない(セッション確立前)。天井を通さないと、
// セッションが不安定なときの refreshWithRetry(4回×指数バックオフ)が
// 唯一の無制限経路になる。
func TestTachibanaLoginGoesThroughRateLimiter(t *testing.T) {
	tb, _, ts := newMockTachibana(t, false, defaultRespond)
	defer ts.Close()

	f := newFakeSleeper(time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC))
	tb.SetRateLimit(2, 1) // 500ms spacing, one burst token
	tb.limiter.sleepFn = f.sleep
	tb.limiter.clock = f.clock()

	ctx := context.Background()
	for i := 0; i < 3; i++ { // RefreshToken = logout + login
		if err := tb.RefreshToken(ctx); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if len(f.waits()) == 0 {
		t.Fatal("login/logout が天井を通っていない")
	}
}

// Default wiring must be limited: an adapter built without an explicit opt-out
// cannot be allowed to poll unbounded.
func TestNewTachibanaIsRateLimitedByDefault(t *testing.T) {
	tb := NewTachibana("demo", "id", testRSAKey(t), "pw", false, false, nil)
	if tb.limiter == nil {
		t.Fatal("NewTachibana must install a rate limiter by default")
	}
}

func TestSetRateLimitOverridesTheDefault(t *testing.T) {
	tb := NewTachibana("demo", "id", testRSAKey(t), "pw", false, false, nil)
	tb.SetRateLimit(0, 0) // explicit opt-out
	if tb.limiter != nil {
		t.Fatal("SetRateLimit(0) must disable limiting")
	}
	tb.SetRateLimit(5, 3)
	if tb.limiter == nil || tb.limiter.spacing != 200*time.Millisecond {
		t.Fatalf("SetRateLimit(5) must give 200ms spacing, got %+v", tb.limiter)
	}
}
