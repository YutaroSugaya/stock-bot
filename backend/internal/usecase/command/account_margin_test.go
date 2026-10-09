package command

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
)

// stubMarginSource は照会回数を数えるだけの口座照会。立花では 1 回 = wire 3 リクエスト
// (買付余力 + 建余力 + 保証金率)なので、ここの 1 は実際には 3 回飛ぶ。
type stubMarginSource struct {
	calls atomic.Int64
	am    *order.AccountMargin
	err   error
}

func (s *stubMarginSource) GetAccountMargin(context.Context) (*order.AccountMargin, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	return s.am, nil
}

func movableClock(t time.Time) (clock.Clock, func(time.Duration)) {
	cur := t
	return func() time.Time { return cur }, func(d time.Duration) { cur = cur.Add(d) }
}

func testMargin() *order.AccountMargin {
	return &order.AccountMargin{AvailableJPY: 500000, MarginNewJPY: 400000, Equity: 560000, MarginRatio: 3.1}
}

// 🚨 実測がこのテストの理由: entry 経路が銘柄ごと・ティックごとに
// 口座照会を払い、後場だけで千回超(= wire 数千回)飛んだ。口座単位の量なので
// 2 回目以降は wire に出てはならない。
func TestAccountMarginCache_SecondReadCostsNoQuery(t *testing.T) {
	src := &stubMarginSource{am: testMargin()}
	clk, _ := movableClock(time.Date(2026, 9, 4, 10, 0, 0, 0, clock.JST))
	c := NewAccountMarginCache(src, time.Hour, clk)

	for i := 0; i < 50; i++ {
		if am, ok := c.Get(context.Background()); !ok || am.Equity != 560000 {
			t.Fatalf("read %d: ok=%v am=%v", i, ok, am)
		}
	}
	if got := src.calls.Load(); got != 1 {
		t.Fatalf("50 回読んで口座照会が %d 回 (want 1)", got)
	}
}

// 約定 / 決済で口座は変わる。そこだけは必ず訊き直す。
func TestAccountMarginCache_InvalidateForcesNextQuery(t *testing.T) {
	src := &stubMarginSource{am: testMargin()}
	clk, _ := movableClock(time.Date(2026, 9, 4, 10, 0, 0, 0, clock.JST))
	c := NewAccountMarginCache(src, time.Hour, clk)

	c.Get(context.Background())
	c.Get(context.Background())
	c.Invalidate()
	c.Get(context.Background())
	if got := src.calls.Load(); got != 2 {
		t.Fatalf("Invalidate 後の照会が %d 回 (want 2 = 初回 + 無効化後の 1 回)", got)
	}
}

// 維持率ブレーカーは口座を見に行くことが目的なので **間引かない**。Refresh は必ず wire。
func TestAccountMarginCache_RefreshAlwaysQueriesAndFeedsTheCache(t *testing.T) {
	src := &stubMarginSource{am: testMargin()}
	clk, _ := movableClock(time.Date(2026, 9, 4, 10, 0, 0, 0, clock.JST))
	c := NewAccountMarginCache(src, time.Hour, clk)

	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := src.calls.Load(); got != 2 {
		t.Fatalf("Refresh 2 回で照会が %d 回 (want 2 — ブレーカーは間引かない)", got)
	}
	// Refresh の結果はキャッシュに入る = entry 経路はタダで新しい値を読める。
	if _, ok := c.Get(context.Background()); !ok {
		t.Fatal("Refresh の結果が Get から読めない")
	}
	if got := src.calls.Load(); got != 2 {
		t.Fatalf("Refresh 直後の Get が追加照会を打った (calls=%d)", got)
	}
}

// イベントが一度も来ない日でも値が丸一日固まらないようにする最後の栓。
func TestAccountMarginCache_ExpiresAfterMaxAge(t *testing.T) {
	src := &stubMarginSource{am: testMargin()}
	clk, advance := movableClock(time.Date(2026, 9, 4, 10, 0, 0, 0, clock.JST))
	c := NewAccountMarginCache(src, time.Hour, clk)

	c.Get(context.Background())
	advance(59 * time.Minute)
	c.Get(context.Background())
	if got := src.calls.Load(); got != 1 {
		t.Fatalf("maxAge 内で %d 回照会した (want 1)", got)
	}
	advance(2 * time.Minute)
	c.Get(context.Background())
	if got := src.calls.Load(); got != 2 {
		t.Fatalf("maxAge 経過後に訊き直していない (calls=%d, want 2)", got)
	}
}

// 🛑 失敗はキャッシュしない(不明を「充分」と読ませない)。ただし障害中に毎ティック
// 再試行すると、削ったはずの通信が障害の形で戻ってくる。再試行には床を敷く。
func TestAccountMarginCache_FailureIsUnknownAndRetryHasAFloor(t *testing.T) {
	src := &stubMarginSource{err: errors.New("session inactive")}
	clk, advance := movableClock(time.Date(2026, 9, 4, 10, 0, 0, 0, clock.JST))
	c := NewAccountMarginCache(src, time.Hour, clk)

	if _, ok := c.Get(context.Background()); ok {
		t.Fatal("照会に失敗したのに ok=true — 不明が『充分』に化ける")
	}
	for i := 0; i < 20; i++ {
		c.Get(context.Background())
	}
	if got := src.calls.Load(); got != 1 {
		t.Fatalf("失敗直後の再試行が %d 回 (want 1 — 床が効いていない)", got)
	}
	advance(accountMarginRetryFloor + time.Second)
	c.Get(context.Background())
	if got := src.calls.Load(); got != 2 {
		t.Fatalf("床を過ぎても再試行していない (calls=%d, want 2)", got)
	}
}
