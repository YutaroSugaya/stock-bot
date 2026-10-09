package main

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/app"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/safety"
	"stockbot/backend/internal/testutil"
	"stockbot/backend/internal/usecase/command"
)

// 🛑 live で場中に建つ戦略は bnf_day2_reversion_trail が初めて(bnf は寄りで建つ)。
// live の arm 済み銘柄の price loop が、当日の現在値が −12% を割った tick で発注まで
// 届くことを、本物の戦略(engine 登録)と live の arm 経路(configForSymbol + liveArmTemplate)で確かめる。
func TestLiveDay2TrailEntersIntradayWhenPriceBreaksThreshold(t *testing.T) {
	ctx := context.Background()
	hours := session.TradingHours{
		TZ:          clock.JST,
		Sessions:    []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:30"}},
		EntryCutoff: "14:55",
		ForceFlatAt: "14:50",
	}
	now := time.Date(2026, 10, 2, 10, 30, 0, 0, clock.JST)
	c := clock.Fixed(now)

	// 前日の確定足が前兆: 出来高 2 倍・乖離は約 −7%(まだパニックでない)。
	cr := repository.NewInMemoryCandleRepo()
	var cs []market.Candle
	for i := 40; i >= 2; i-- {
		d := time.Date(2026, 10, 2, 0, 0, 0, 0, clock.JST).AddDate(0, 0, -i)
		cs = append(cs, market.Candle{Symbol: "7203", Interval: 24 * time.Hour, OpenTime: d,
			Open: 2000, High: 2040, Low: 1960, Close: 2000, Volume: 1000})
	}
	prev := time.Date(2026, 10, 1, 0, 0, 0, 0, clock.JST)
	cs = append(cs, market.Candle{Symbol: "7203", Interval: 24 * time.Hour, OpenTime: prev,
		Open: 1990, High: 2000, Low: 1850, Close: 1860, Volume: 2000})
	if err := cr.Upsert(ctx, "7203", cs); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	tmpl := &config.StrategyConfig{
		ConfigID: "live_bnf_day2_reversion_trail_v1", Symbol: "*",
		StrategyName: config.StrategyBNFDay2ReversionTrail, Mode: config.ModeLive,
		HoldingMode: config.HoldingMultiday, ExecKind: "margin_system",
	}
	tmpl.Entry.Direction = config.DirectionBuyOnly
	tmpl.Entry.MaxSpreadTicks = 5
	tmpl.Risk.Quantity = 100
	tmpl.Risk.MaxOpenPositions = 1
	tmpl.Risk.MaxTradesInThisWindow = 3
	tmpl.Risk.MaxLossInThisWindowJPY = 8000
	armed := configForSymbol(liveArmTemplate(tmpl), "7203")

	pb := broker.NewPaper(c, 0, 0)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(t.TempDir()+"/f.flag", nil)
	closer := repository.NewCloser(posRepo, tradeRepo)
	exec := command.NewExecuteOrder(pb, posRepo, safety.NewPendingPositions(), es, c).WithCloser(closer)
	snap := command.NewSnapshotBuilder(posRepo, tradeRepo, pb, es, hours, c,
		command.SnapshotCaps{MaxRiskPerTradeJPY: 100000})
	cycle := command.NewTradingCycle(buildStrategyEngine(func() string { return "sig" }), snap, exec, 100,
		func(order.HoldingMode) order.ExecKind { return order.ExecMarginSystem }, hours)

	cfgs := app.NewConfigSet(defaultNoTradeConfig("7203", config.ModeLive))
	cfgs.Arm(armed)
	b := &app.SymbolBundle{
		Symbol: "7203", Broker: pb, Agg: market.NewAggregator("7203", 64), Candles: cr,
		Configs: cfgs, Cycle: cycle, Hours: hours, Clock: c,
		Manage:            command.NewManageOpenPositions(pb, posRepo, closer, es, c, 0.30),
		Flatten:           command.NewForceFlatten(pb, posRepo, closer, es, hours, c),
		Counters:          &app.Counters{},
		Logger:            testutil.SilentLogger(),
		PosRepo:           posRepo,
		AllowHistoryFetch: func(time.Time) bool { return false },
	}

	// 25 日線 ≒ 1994。現在値 1,850 は乖離 −7% で、まだ建たない。
	pb.SetPrice("7203", 1850)
	b.PriceTick(ctx)
	if open, _ := posRepo.ListOpenOrClosing(ctx, "7203"); len(open) != 0 {
		t.Fatalf("−12%% を割る前に建った: %d 本", len(open))
	}

	// 1,740 は乖離 −12.7%。この tick で建つ。
	pb.SetPrice("7203", 1740)
	b.PriceTick(ctx)
	open, err := posRepo.ListOpenOrClosing(ctx, "7203")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("−12%% を割った tick で建たない: %d 本", len(open))
	}
	p := open[0]
	if p.StrategyName != string(config.StrategyBNFDay2ReversionTrail) {
		t.Fatalf("戦略 = %s", p.StrategyName)
	}
	if p.HoldingMode != order.HoldingMultiday || p.TakeProfitJPY != 0 || p.StopLossJPY <= 0 || p.RatchetArmJPY <= 0 {
		t.Fatalf("trail の出口が凍結されていない: %+v", p)
	}
}
