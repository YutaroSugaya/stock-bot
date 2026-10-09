package broker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// recordingFeed は read-only フィードの fake。port.MarketFeed しか実装しないので、
// 「決して発注 API を呼べない」ことが型で保証されていることも同時に示す。
type recordingFeed struct {
	last      float64
	klines    []market.Candle
	refreshes int
	tickers   int
}

func (f *recordingFeed) GetTicker(_ context.Context, symbol string) (*market.Ticker, error) {
	f.tickers++
	return &market.Ticker{Symbol: symbol, Bid: f.last - 0.5, Ask: f.last + 0.5, Last: f.last, Timestamp: time.Now()}, nil
}

func (f *recordingFeed) GetKlines(_ context.Context, _ string, _ port.KlinePeriod, _ int) ([]market.Candle, error) {
	return f.klines, nil
}

func (f *recordingFeed) RefreshToken(_ context.Context) error { f.refreshes++; return nil }

func newPLF(t *testing.T, last float64) (*PaperLiveFeed, *recordingFeed) {
	t.Helper()
	feed := &recordingFeed{last: last, klines: []market.Candle{{Close: last}}}
	p := NewPaper(clock.System(), 0, 0)
	p.SetPrice("7203", 2500) // paper の種価格(実フィードで上書きされること)
	return NewPaperLiveFeed(p, feed), feed
}

// 実フィードの価格が paper の約定価格になる(種価格 2500 のままだと検証が無意味)。
func TestPaperLiveFeedUsesRealPriceForFills(t *testing.T) {
	plf, feed := newPLF(t, 3000)
	ctx := context.Background()

	tk, err := plf.GetTicker(ctx, "7203")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if tk.Last != 3000 {
		t.Fatalf("ticker last = %v, want 3000 (実フィード)", tk.Last)
	}
	if feed.tickers != 1 {
		t.Fatalf("feed GetTicker calls = %d, want 1", feed.tickers)
	}

	res, err := plf.PlaceOrder(ctx, order.PlaceOrderRequest{
		Symbol: "7203", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100, ExecKind: order.ExecCash,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	ex, err := plf.ResolveExecution(ctx, res.OrderID)
	if err != nil {
		t.Fatalf("ResolveExecution: %v", err)
	}
	if ex.FilledPrice != 3000 {
		t.Fatalf("filled price = %v, want 3000 (実フィード価格で約定); 種価格 2500 のままなら結線漏れ", ex.FilledPrice)
	}
}

// 建玉/余力は paper 側(紙の帳簿)。実口座の建玉が混ざってはいけない。
func TestPaperLiveFeedBooksArePaper(t *testing.T) {
	plf, _ := newPLF(t, 3000)
	ctx := context.Background()
	if _, err := plf.PlaceOrder(ctx, order.PlaceOrderRequest{
		Symbol: "7203", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100, ExecKind: order.ExecCash,
	}); err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	ps, err := plf.GetPositions(ctx)
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("positions = %d, want 1 (paper の帳簿)", len(ps))
	}
	if _, err := plf.GetAccountMargin(ctx); err != nil {
		t.Fatalf("GetAccountMargin: %v", err)
	}
}

// 日足は実フィードから(paper は日足を返せない)。
func TestPaperLiveFeedKlinesFromFeed(t *testing.T) {
	plf, _ := newPLF(t, 3000)
	kl, err := plf.GetKlines(context.Background(), "7203", port.PeriodDaily, 60)
	if err != nil {
		t.Fatalf("GetKlines: %v", err)
	}
	if len(kl) != 1 || kl[0].Close != 3000 {
		t.Fatalf("klines = %+v, want feed の1本", kl)
	}
}

// RefreshToken は実フィードのセッションを維持する(立花は日次で切れる)。
func TestPaperLiveFeedRefreshesFeedSession(t *testing.T) {
	plf, feed := newPLF(t, 3000)
	if err := plf.RefreshToken(context.Background()); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if feed.refreshes != 1 {
		t.Fatalf("feed refreshes = %d, want 1", feed.refreshes)
	}
}

// port.LiveBroker を満たす(守りの二段構えを含む)。
// 🛑 チャンク数はデコレータ 2 枚(PaperLiveFeed → BatchQuoteFeed)の**内側**にある。
// 素通し経路が無いと、監視 120 銘柄超えの警告と予算計算が本番だけ「常に 1」で回る。
func TestPaperLiveFeedForwardsQuoteChunks(t *testing.T) {
	f := newFakeFeed(map[string]float64{})
	f.batchSize = 120
	bf := NewBatchQuoteFeed(f, time.Second, f.clock()).(*BatchQuoteFeed)
	plf := NewPaperLiveFeed(NewPaper(f.clock(), 0, 0), bf)

	if got := QuoteChunks(plf); got != 1 {
		t.Fatalf("監視ゼロで chunks=%d, want 1", got)
	}
	bf.mu.Lock()
	for i := 0; i < 241; i++ {
		bf.want[fmt.Sprintf("%d", 1000+i)] = f.now
	}
	bf.mu.Unlock()
	if got := QuoteChunks(plf); got != 3 {
		t.Fatalf("241銘柄で chunks=%d, want 3 — デコレータ越しに伝わっていない", got)
	}
}

// 一括に対応しないフィードは 1(不明なら安全側の「増えていない」ではなく、
// 少なくとも 1 リクエストとして数える)。
func TestQuoteChunksDefaultsToOne(t *testing.T) {
	if got := QuoteChunks(plainFeed{}); got != 1 {
		t.Fatalf("申告しないフィードで chunks=%d, want 1", got)
	}
}

func TestPaperLiveFeedSatisfiesLiveBroker(t *testing.T) {
	var _ port.LiveBroker = (*PaperLiveFeed)(nil)
	var _ port.MarketFeed = (*Tachibana)(nil) // 立花はフィードとして使える
}

// フィード障害は伝播させる(paper の古い価格で約定させない)。
func TestPaperLiveFeedPropagatesFeedError(t *testing.T) {
	p := NewPaper(clock.System(), 0, 0)
	plf := NewPaperLiveFeed(p, errFeed{})
	if _, err := plf.GetTicker(context.Background(), "7203"); err == nil {
		t.Fatal("フィード障害が握り潰された(古い価格で約定しかねない)")
	}
}

// Stale(寄り前・売買停止の前日終値 indicative)は紙の帳簿に入れない:
// 引け前フラット化など stale でも走る経路が実勢でない価格で約定を記録するため。
func TestPaperLiveFeedIgnoresStalePrice(t *testing.T) {
	p := NewPaper(clock.System(), 0, 0)
	plf := NewPaperLiveFeed(p, staleFeed{last: 3000})
	ctx := context.Background()
	if _, err := plf.GetTicker(ctx, "7203"); err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if _, err := plf.PlaceOrder(ctx, order.PlaceOrderRequest{
		Symbol: "7203", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100, ExecKind: order.ExecCash,
	}); err == nil {
		t.Fatal("stale 価格で紙約定した(paper 損益が実勢と乖離する)")
	}
}

type errFeed struct{}

func (errFeed) GetTicker(context.Context, string) (*market.Ticker, error) {
	return nil, errTestFeed
}
func (errFeed) GetKlines(context.Context, string, port.KlinePeriod, int) ([]market.Candle, error) {
	return nil, errTestFeed
}
func (errFeed) RefreshToken(context.Context) error { return errTestFeed }

var errTestFeed = errors.New("feed down")

type staleFeed struct{ last float64 }

func (f staleFeed) GetTicker(_ context.Context, sym string) (*market.Ticker, error) {
	return &market.Ticker{Symbol: sym, Last: f.last, Stale: true, Timestamp: time.Now()}, nil
}
func (staleFeed) GetKlines(context.Context, string, port.KlinePeriod, int) ([]market.Candle, error) {
	return nil, nil
}
func (staleFeed) RefreshToken(context.Context) error { return nil }

// 復元は合成 broker からも届く必要がある(main が握るのは PaperLiveFeed 側)。
func TestPaperLiveFeedForwardsAdoptOpenPositions(t *testing.T) {
	b, _ := newPLF(t, 3100)
	if n := b.AdoptOpenPositions([]port.BrokerPosition{{
		BrokerPositionID: "bp-2", Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 3000,
	}}); n != 1 {
		t.Fatalf("AdoptOpenPositions = %d, want 1", n)
	}
	// 実フィードのティックを一度通してから決済(紙の価格は GetTicker で入る)。
	if _, err := b.GetTicker(context.Background(), "7203"); err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	res, err := b.ClosePosition(context.Background(), port.CloseRequest{
		Symbol: "7203", BrokerPositionID: "bp-2", Side: order.SideSell, Quantity: 100})
	if err != nil || !res.Accepted {
		t.Fatalf("復元建玉が合成 broker 経由で決済できない: %+v %v", res, err)
	}
}
