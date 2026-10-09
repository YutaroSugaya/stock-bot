package app

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
	"stockbot/backend/internal/testutil"
	"stockbot/backend/internal/usecase/command"
)

// 🛑 ペア比較の**着手条件**: 「同一トリガーで 2 アームが同一ラウンドで
// arm され、**同一ティックで建つ(建値が同じ)**」。これが通らない限りペアは
// 設計上のみで、bnf ペア 0 本の壊れ方を再現する。

// 入口はいつでも成立し、出口だけが違う 2 つのダミー戦略。日足フィクスチャに依存せず
// 「bundle が 2 本とも実行するか」だけを見る。
type alwaysEnter struct {
	name config.StrategyName
	tp   float64
}

func (s alwaysEnter) Name() config.StrategyName { return s.name }

func (s alwaysEnter) Evaluate(in strategy.EvalInput) strategy.Signal {
	return strategy.Signal{
		Decision: strategy.DecisionEnter, Symbol: in.Config.Symbol, Side: order.SideBuy,
		EntryPrice: in.Summary.CurrentRate.Last, Quantity: 100,
		TakeProfitJPY: s.tp, StopLossJPY: 50, HoldingMode: order.HoldingMultiday,
		ConfigID: in.Config.ConfigID, StrategyName: s.name, CreatedAt: in.Now,
	}
}

// 🚨 **本番の config を使う**(監査で発覚した欠陥の再発防止)。
//
// 以前ここは config を手で組み立てており、`Risk.MaxOpenPositions` を**未設定
// (0 = 無制限)**にしていた。そのせいで「兄弟アームの 2 本目が銘柄単位の建玉枠で
// 必ず落ちる」という**本番だけの欠陥**をこのテストが見逃していた ——
// 「着手条件」として要求した guard test が、本番設定を縛れていなかった。
//
// 決定論テンプレート(= 実際に arm される config)をそのまま使う。
func pairCfg(t *testing.T, name config.StrategyName) *config.StrategyConfig {
	t.Helper()
	hl := &config.HardLimits{AllowedSymbols: []string{"7203"}}
	hl.Quantity = config.IntRange{Min: 100, Max: 3000}
	hl.MaxTradesInThisWindow = config.IntRange{Min: 1, Max: 5}
	hl.MaxLossInWindowJPY = config.IntRange{Min: 1000, Max: 1000000}
	hl.MaxSpreadTicks = config.FloatRange{Min: 1, Max: 10}
	tmpl := &command.ArmTemplate{
		HardLimits: hl, Mode: config.ModePaper,
		ExecKindFor: func(order.HoldingMode) order.ExecKind { return order.ExecCash },
	}
	c, err := tmpl.Build("7203", name, 2000, time.Date(2026, 8, 24, 9, 0, 0, 0, clock.JST))
	if err != nil {
		t.Fatalf("arm template %s: %v", name, err)
	}
	// 🛑 この 1 行がテストの意味。`max_open_positions: 1` が凍結されていることを
	// 確かめてから回す — ここが 0(無制限)に変わると、テストは緑のまま
	// 「銘柄単位の枠が 2 本目を落とす」欠陥を素通しするようになる。
	if c.Risk.MaxOpenPositions != 1 {
		t.Fatalf("前提: テンプレートは max_open_positions=1 を凍結する(got %d)。"+
			"この値が変わったなら、枠が (銘柄, 戦略) キーであることを別の形で縛り直すこと",
			c.Risk.MaxOpenPositions)
	}
	return c
}

// 値幅制限ゲート(risk.EvaluatePriceLimit)は前日終値を要求する。日足が無いと
// price_limit_reference_unavailable で全部 reject されるので、平坦な系列を用意する。
func pairCandles(t *testing.T, upTo time.Time) *repository.InMemoryCandleRepo {
	t.Helper()
	cr := repository.NewInMemoryCandleRepo()
	cs := make([]market.Candle, 0, 30)
	for i := 29; i >= 1; i-- {
		d := upTo.AddDate(0, 0, -i)
		cs = append(cs, market.Candle{
			Symbol: "7203", Interval: 24 * time.Hour, OpenTime: d,
			Open: 2000, High: 2010, Low: 1990, Close: 2000, Volume: 1000,
		})
	}
	if err := cr.Upsert(context.Background(), "7203", cs); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return cr
}

func TestPriceTickEntersBothArmsOnTheSameTickAtTheSamePrice(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, clock.JST)
	c := clock.Fixed(now)

	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("7203", 2000)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(t.TempDir()+"/f.flag", nil)

	capped := alwaysEnter{name: config.StrategyBNFReversion, tp: 100}
	trail := alwaysEnter{name: config.StrategyBNFReversionTrail, tp: 0}
	eng := strategy.NewEngine(func() string { return "sig" }, capped, trail)

	exec := command.NewExecuteOrder(pb, posRepo, safety.NewPendingPositions(), es, c).
		WithCloser(repository.NewCloser(posRepo, tradeRepo))
	snap := command.NewSnapshotBuilder(posRepo, tradeRepo, pb, es, tokyoHours(), c, command.SnapshotCaps{})
	cycle := command.NewTradingCycle(eng, snap, exec, 100,
		func(order.HoldingMode) order.ExecKind { return order.ExecCash }, tokyoHours())

	cs := NewConfigSet(pairCfg(t, config.StrategyNoTrade))
	cs.Arm(pairCfg(t, config.StrategyBNFReversion))
	cs.Arm(pairCfg(t, config.StrategyBNFReversionTrail))

	b := &SymbolBundle{
		Symbol: "7203", Broker: pb, Agg: market.NewAggregator("7203", 64),
		Candles: pairCandles(t, now),
		Configs: cs, Cycle: cycle, Hours: tokyoHours(), Clock: c,
		Manage:            command.NewManageOpenPositions(pb, posRepo, repository.NewCloser(posRepo, tradeRepo), es, c, 0.30),
		Flatten:           command.NewForceFlatten(pb, posRepo, repository.NewCloser(posRepo, tradeRepo), es, tokyoHours(), c),
		Counters:          &Counters{},
		Logger:            testutil.SilentLogger(),
		PosRepo:           posRepo,
		AllowHistoryFetch: func(time.Time) bool { return false },
	}
	b.PriceTick(ctx)

	open, err := posRepo.ListOpenOrClosing(ctx, "7203")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("同一ティックで 2 本建つこと(着手条件): %d 本", len(open))
	}
	if open[0].EntryPrice != open[1].EntryPrice {
		t.Fatalf("建値が違う: %v vs %v — ペアの前提が崩れている", open[0].EntryPrice, open[1].EntryPrice)
	}
	got := map[string]bool{open[0].StrategyName: true, open[1].StrategyName: true}
	if !got[string(config.StrategyBNFReversion)] || !got[string(config.StrategyBNFReversionTrail)] {
		t.Fatalf("2 本が別戦略になっていない: %v", got)
	}
}

// 同一戦略の 2 度目はナンピン禁止で止まる(緩めたのは「別戦略か」の 1 点だけ)。
func TestPriceTickStillBlocksTheSameStrategyTwice(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, clock.JST)
	c := clock.Fixed(now)

	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("7203", 2000)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(t.TempDir()+"/f.flag", nil)

	eng := strategy.NewEngine(func() string { return "sig" },
		alwaysEnter{name: config.StrategyBNFReversion, tp: 100})
	exec := command.NewExecuteOrder(pb, posRepo, safety.NewPendingPositions(), es, c).
		WithCloser(repository.NewCloser(posRepo, tradeRepo))
	snap := command.NewSnapshotBuilder(posRepo, tradeRepo, pb, es, tokyoHours(), c, command.SnapshotCaps{})
	cycle := command.NewTradingCycle(eng, snap, exec, 100,
		func(order.HoldingMode) order.ExecKind { return order.ExecCash }, tokyoHours())

	cs := NewConfigSet(pairCfg(t, config.StrategyNoTrade))
	cs.Arm(pairCfg(t, config.StrategyBNFReversion))

	b := &SymbolBundle{
		Symbol: "7203", Broker: pb, Agg: market.NewAggregator("7203", 64),
		Candles: pairCandles(t, now),
		Configs: cs, Cycle: cycle, Hours: tokyoHours(), Clock: c,
		Manage:            command.NewManageOpenPositions(pb, posRepo, repository.NewCloser(posRepo, tradeRepo), es, c, 0.30),
		Flatten:           command.NewForceFlatten(pb, posRepo, repository.NewCloser(posRepo, tradeRepo), es, tokyoHours(), c),
		Counters:          &Counters{},
		Logger:            testutil.SilentLogger(),
		PosRepo:           posRepo,
		AllowHistoryFetch: func(time.Time) bool { return false },
	}
	b.PriceTick(ctx)
	b.PriceTick(ctx)

	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 {
		t.Fatalf("同一戦略が積み増された: %d 本(ナンピン禁止は hard gate)", len(open))
	}
}

var _ port.PositionRepository = (*repository.InMemoryPositionRepo)(nil)
