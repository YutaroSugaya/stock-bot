package broker

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// countingLive は wire 呼び出しの回数を数える LiveBroker。
// MOCK rationale (TESTING.md §1 system boundary): 立花への口座照会・発注は wire なので、
// 「畳まれたか」「素通ししたか」は数え口を挟む以外に観測できない。
type countingLive struct {
	*Paper
	positions, margins, orders, cancels int
	tickers                             int
}

func (c *countingLive) GetPositions(ctx context.Context) ([]port.BrokerPosition, error) {
	c.positions++
	return c.Paper.GetPositions(ctx)
}

func (c *countingLive) GetAccountMargin(ctx context.Context) (*order.AccountMargin, error) {
	c.margins++
	return &order.AccountMargin{AvailableJPY: 900000, MarginRatio: 1.0, Equity: 900000}, nil
}

func (c *countingLive) PlaceOrder(ctx context.Context, req order.PlaceOrderRequest) (*port.PlaceOrderResult, error) {
	c.orders++
	return c.Paper.PlaceOrder(ctx, req)
}

func (c *countingLive) CancelOrder(ctx context.Context, id string) (*port.CancelResult, error) {
	c.cancels++
	return c.Paper.CancelOrder(ctx, id)
}

func (c *countingLive) GetTicker(ctx context.Context, sym string) (*market.Ticker, error) {
	c.tickers++
	return c.Paper.GetTicker(ctx, sym)
}

// feedStub は共有フィード役。live track は時価をここから受け取る(= 追加の
// wire コストゼロ)。
type feedStub struct {
	price float64
	calls int
}

func (f *feedStub) GetTicker(_ context.Context, sym string) (*market.Ticker, error) {
	f.calls++
	return &market.Ticker{Symbol: sym, Bid: f.price, Ask: f.price, Last: f.price}, nil
}

func (f *feedStub) GetKlines(context.Context, string, port.KlinePeriod, int) ([]market.Candle, error) {
	return nil, nil
}
func (f *feedStub) RefreshToken(context.Context) error { return nil }
func (f *feedStub) QuotesBatched() bool                { return true }

func newShared(t *testing.T, now time.Time) (*countingLive, *feedStub, port.LiveBroker) {
	t.Helper()
	c := clock.Fixed(now)
	tb := &countingLive{Paper: NewPaper(c, 0, 0)}
	feed := &feedStub{price: 2500}
	return tb, feed, NewLiveQuoteShared(tb, feed, time.Minute, c)
}

// 時価は共有フィードから来る。live track が何銘柄増えても時価レーンの本数は
// 変わらない — 素通しにすると 1銘柄1リクエストに戻る。
func TestLiveQuoteShared_TickerComesFromFeed(t *testing.T) {
	tb, feed, s := newShared(t, time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST))
	tk, err := s.GetTicker(context.Background(), "7203")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if tk.Last != 2500 {
		t.Errorf("price=%v, want 2500", tk.Last)
	}
	if feed.calls != 1 || tb.tickers != 0 {
		t.Errorf("feed=%d tb=%d — 時価は feed から取り、tb を直接叩いてはいけない", feed.calls, tb.tickers)
	}
}

// QuotesBatched が true でないと defaultLoopConfig が 30 秒へ退避する
// (exec_broker.go の「live は一括化されないので 30 秒に落ちる」問題の解)。
func TestLiveQuoteShared_ReportsBatched(t *testing.T) {
	_, _, s := newShared(t, time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST))
	if !QuotesBatched(s) {
		t.Fatal("QuotesBatched=false — 価格ループが 30 秒に退避してしまう")
	}
}

// 🛑 口座照会は TTL 窓内で 1 リクエストに畳む。`GetPositions` は**口座全体**を返す
// 照会を呼び出し側が symbol で絞っているだけなので、素通しにすると N 銘柄が同じ
// 照会を N 回投げる(時価の BatchQuoteFeed とまったく同じ構図)。
func TestLiveQuoteShared_AccountQueriesAreMerged(t *testing.T) {
	tb, _, s := newShared(t, time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST))
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if _, err := s.GetPositions(ctx); err != nil {
			t.Fatalf("GetPositions: %v", err)
		}
		if _, err := s.GetAccountMargin(ctx); err != nil {
			t.Fatalf("GetAccountMargin: %v", err)
		}
	}
	if tb.positions != 1 {
		t.Errorf("建玉照会 %d回 — TTL 窓内は 1 回に畳むべき", tb.positions)
	}
	if tb.margins != 1 {
		t.Errorf("余力照会 %d回 — TTL 窓内は 1 回に畳むべき", tb.margins)
	}
}

// TTL が切れれば取り直す(古い建玉を新鮮なふりで返さない)。
func TestLiveQuoteShared_AccountCacheExpires(t *testing.T) {
	at := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	now := at
	c := clock.Clock(func() time.Time { return now })
	tb := &countingLive{Paper: NewPaper(c, 0, 0)}
	s := NewLiveQuoteShared(tb, &feedStub{price: 2500}, time.Minute, c)
	ctx := context.Background()

	_, _ = s.GetPositions(ctx)
	now = now.Add(30 * time.Second)
	_, _ = s.GetPositions(ctx)
	if tb.positions != 1 {
		t.Fatalf("TTL 内で %d回, want 1", tb.positions)
	}
	now = now.Add(31 * time.Second)
	_, _ = s.GetPositions(ctx)
	if tb.positions != 2 {
		t.Fatalf("TTL 経過後 %d回, want 2", tb.positions)
	}
}

// 🛑 **発注系は絶対にキャッシュしない**。状態変更は毎回 tb へ届かなければならない。
func TestLiveQuoteShared_OrdersAreNeverCached(t *testing.T) {
	tb, _, s := newShared(t, time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST))
	ctx := context.Background()
	tb.Paper.SetPrice("7203", 2500)
	for i := 0; i < 3; i++ {
		if _, err := s.PlaceOrder(ctx, order.PlaceOrderRequest{
			Symbol: "7203", Side: order.SideBuy, Quantity: 100, Type: order.OrderTypeMarket,
			ExecKind: order.ExecCash,
		}); err != nil {
			t.Fatalf("PlaceOrder: %v", err)
		}
		if _, err := s.CancelOrder(ctx, "o-1"); err != nil {
			t.Fatalf("CancelOrder: %v", err)
		}
	}
	if tb.orders != 3 {
		t.Errorf("PlaceOrder が %d回しか届いていない, want 3(発注は畳まない)", tb.orders)
	}
	if tb.cancels != 3 {
		t.Errorf("CancelOrder が %d回しか届いていない, want 3", tb.cancels)
	}
}

// 🛑 発注・決済のあとは口座キャッシュを**捨てる**。畳んだ結果として「建てたのに
// 建玉ゼロ」「決済したのに残っている」を返すと、ナンピン禁止ゲートと Reconcile が
// 古い像で判断する。**通信量のための併合が、守りの誤作動になってはいけない。**
func TestLiveQuoteShared_StateChangeInvalidatesAccountCache(t *testing.T) {
	tb, _, s := newShared(t, time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST))
	ctx := context.Background()
	tb.Paper.SetPrice("7203", 2500)

	_, _ = s.GetPositions(ctx)
	if tb.positions != 1 {
		t.Fatalf("前提: %d回", tb.positions)
	}
	if _, err := s.PlaceOrder(ctx, order.PlaceOrderRequest{
		Symbol: "7203", Side: order.SideBuy, Quantity: 100, Type: order.OrderTypeMarket,
		ExecKind: order.ExecCash,
	}); err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	_, _ = s.GetPositions(ctx)
	if tb.positions != 2 {
		t.Errorf("発注後も古い建玉像を返している(%d回) — 状態変更でキャッシュを捨てるべき", tb.positions)
	}
}

var _ port.LiveBroker = (*countingLive)(nil)
var _ port.MarketFeed = (*feedStub)(nil)
