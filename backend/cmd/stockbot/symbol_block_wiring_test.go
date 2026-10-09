package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/adapter/symbolblock"
	"stockbot/backend/internal/app"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
	"stockbot/backend/internal/testutil"
)

// enterEveryTick は毎ティック建てようとする stub 戦略(配線だけを見る)。
type enterEveryTick struct{}

func (enterEveryTick) Name() config.StrategyName { return config.StrategyBNFDay2ReversionTrail }

func (enterEveryTick) Evaluate(in strategy.EvalInput) strategy.Signal {
	return strategy.Signal{
		Decision: strategy.DecisionEnter, Symbol: in.Config.Symbol, Side: order.SideBuy,
		EntryPrice: in.Summary.CurrentRate.Last, Quantity: 100, StopLossJPY: 50,
		HoldingMode: order.HoldingMultiday, ConfigID: in.Config.ConfigID,
		StrategyName: config.StrategyBNFDay2ReversionTrail, CreatedAt: in.Now,
	}
}

type captureRejections struct {
	mu      sync.Mutex
	reasons []string
}

func (c *captureRejections) InsertRejection(_ context.Context, r port.SignalRejection) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reasons = append(c.reasons, r.Reason)
	return nil
}

type blockFixture struct {
	bundle  *app.SymbolBundle
	broker  *broker.Paper
	posRepo *repository.InMemoryPositionRepo
	rej     *captureRejections
}

func (f blockFixture) open(t *testing.T) int {
	t.Helper()
	open, err := f.posRepo.ListOpenOrClosing(context.Background(), "7203")
	if err != nil {
		t.Fatal(err)
	}
	return len(open)
}

// 同じ配線で、停止の store を挿した track(live)だけが止まり、挿さない track(research)は建つ。
func symbolBlockTrack(t *testing.T, blocks port.SymbolBlockReader) (opened int, reasons []string) {
	t.Helper()
	f := newBlockFixture(t, blocks)
	f.bundle.PriceTick(context.Background())
	return f.open(t), f.rej.reasons
}

func newBlockFixture(t *testing.T, blocks port.SymbolBlockReader) blockFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 10, 30, 0, 0, clock.JST)
	c := clock.Fixed(now)
	hours := session.TradingHours{
		TZ:          clock.JST,
		Sessions:    []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:30"}},
		EntryCutoff: "14:55", ForceFlatAt: "14:50",
	}
	cr := repository.NewInMemoryCandleRepo()
	var cs []market.Candle
	for i := 30; i >= 1; i-- {
		cs = append(cs, market.Candle{Symbol: "7203", Interval: 24 * time.Hour,
			OpenTime: time.Date(2026, 10, 2, 0, 0, 0, 0, clock.JST).AddDate(0, 0, -i),
			Open:     2000, High: 2010, Low: 1990, Close: 2000, Volume: 1000})
	}
	if err := cr.Upsert(ctx, "7203", cs); err != nil {
		t.Fatal(err)
	}
	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("7203", 2000)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	rej := &captureRejections{}
	botCfg := &config.BotConfig{Mode: config.ModeLive}
	botCfg.Holding.Multiday.ExecKind = config.ExecMarginSystem
	d := &wiringDeps{
		broker: pb, posRepo: posRepo, tradeRepo: tradeRepo, closer: repository.NewCloser(posRepo, tradeRepo),
		candles: cr, rejections: rej, pending: safety.NewPendingPositions(),
		emergency: safety.NewEmergencyStop(testutil.TempFlagPath(t), nil),
		engine:    strategy.NewEngine(func() string { return "s" }, enterEveryTick{}),
		hours:     hours, clock: c, botCfg: botCfg, hardLimits: &config.HardLimits{AllowedSymbols: []string{"7203"}},
		logger: testutil.SilentLogger(), counters: &app.Counters{}, symbolBlocks: blocks,
	}
	active := &config.StrategyConfig{ConfigID: "live_day2_7203", Symbol: "7203",
		StrategyName: config.StrategyBNFDay2ReversionTrail, Mode: config.ModeLive,
		HoldingMode: config.HoldingMultiday, ExecKind: config.ExecMarginSystem}
	active.Entry.Direction = config.DirectionBuyOnly
	active.Entry.MaxSpreadTicks = 5
	active.Risk.Quantity = 100
	active.Risk.MaxOpenPositions = 1
	b := buildSymbolBundle(d, "7203", active)
	b.AllowHistoryFetch = func(time.Time) bool { return false }
	return blockFixture{bundle: b, broker: pb, posRepo: posRepo, rej: rej}
}

func TestSymbolBlock_LiveTrackRejectsBlockedSymbol(t *testing.T) {
	store := symbolblock.NewFileStore(filepath.Join(t.TempDir(), "live_symbol_blocks.json"), clock.System())
	if err := store.Block(context.Background(), "7203", "test"); err != nil {
		t.Fatal(err)
	}
	opened, reasons := symbolBlockTrack(t, store)
	if opened != 0 {
		t.Fatalf("止めた銘柄が建った: %d 本", opened)
	}
	if len(reasons) == 0 || reasons[0] != risk.ReasonManualSymbolBlock {
		t.Fatalf("見送りの記録 = %v, want %s", reasons, risk.ReasonManualSymbolBlock)
	}
}

// 停止していない銘柄は建つ(store を挿した live でも)/ store を挿さない research は止まらない。
func TestSymbolBlock_UnblockedAndResearchStillEnter(t *testing.T) {
	store := symbolblock.NewFileStore(filepath.Join(t.TempDir(), "live_symbol_blocks.json"), clock.System())
	if err := store.Block(context.Background(), "6594", ""); err != nil {
		t.Fatal(err)
	}
	if opened, reasons := symbolBlockTrack(t, store); opened != 1 {
		t.Fatalf("止めていない銘柄が建たない: reasons=%v", reasons)
	}
	if opened, reasons := symbolBlockTrack(t, nil); opened != 1 {
		t.Fatalf("research が建たない: reasons=%v", reasons)
	}
}

// 止めるのは新規だけ。建玉中の銘柄を止めても、決済(SL)は今までどおり走る。
func TestSymbolBlock_DoesNotStopExits(t *testing.T) {
	ctx := context.Background()
	store := symbolblock.NewFileStore(filepath.Join(t.TempDir(), "live_symbol_blocks.json"), clock.System())
	f := newBlockFixture(t, store)
	f.bundle.PriceTick(ctx)
	if f.open(t) != 1 {
		t.Fatalf("前提: 止める前は建つ: %v", f.rej.reasons)
	}
	if err := store.Block(ctx, "7203", ""); err != nil {
		t.Fatal(err)
	}
	f.broker.SetPrice("7203", 1940) // 建値 2000 − SL 50 を割る
	f.bundle.PriceTick(ctx)
	if n := f.open(t); n != 0 {
		t.Fatalf("止めた銘柄の決済が止まった: 建玉 %d 本", n)
	}
}
