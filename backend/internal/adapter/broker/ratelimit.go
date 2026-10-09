package broker

import (
	"context"
	"sync"
	"time"

	"stockbot/backend/internal/domain/clock"
)

// 🛑 ここを通らない API 経路を作らない。超過分は捨てず待たせる。詳細: docs/runtime/TACHIBANA_API_NOTES.md §1.5

// nil は「無制限」。paper/test の配線は未設定のままでよい。
type rateLimiter struct {
	spacing time.Duration // 1/rps: 定常の呼び出し間隔
	burst   float64       // アイドル後に連続して撃てる本数
	clock   clock.Clock

	// テストは実際に眠らず可動 clock を進める偽物に差し替える。
	sleepFn func(context.Context, time.Duration) error

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// rps <= 0 は明示的な opt-out で nil(無制限)を返す。
func newRateLimiter(rps float64, burst int, c clock.Clock) *rateLimiter {
	if rps <= 0 {
		return nil
	}
	if burst < 1 {
		burst = 1
	}
	if c == nil {
		c = clock.System()
	}
	return &rateLimiter{
		spacing: time.Duration(float64(time.Second) / rps),
		burst:   float64(burst),
		clock:   c,
		sleepFn: sleepCtx,
		tokens:  float64(burst),
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// 枠はロック下で予約し、sleep はロックの外で行う(同じトークンで一斉に起きて天井を突き破らないため)。
func (l *rateLimiter) Wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	now := l.clock()
	if l.last.IsZero() {
		l.last = now
	}
	// 予約済みの未来 last は elapsed が負になる。そこでトークンを戻さない。
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens += float64(elapsed) / float64(l.spacing)
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
	}
	var wait time.Duration
	if l.tokens >= 1 {
		l.tokens--
	} else {
		wait = time.Duration((1 - l.tokens) * float64(l.spacing))
		l.tokens = 0
		l.last = l.last.Add(wait) // 予約。次の caller はこの後ろに並ぶ
	}
	l.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	return l.sleepFn(ctx, wait)
}
