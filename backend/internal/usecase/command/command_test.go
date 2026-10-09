package command

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
	"stockbot/backend/internal/testutil"
)

func tokyoHours() session.TradingHours {
	return session.TradingHours{
		TZ:          clock.JST,
		Sessions:    []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:00"}},
		EntryCutoff: "14:55",
		ForceFlatAt: "14:50",
	}
}

func dailyUptrend(n int) []market.Candle {
	cs := make([]market.Candle, n)
	for i := 0; i < n; i++ {
		p := 2000 + float64(i)*3
		cs[i] = market.Candle{Open: p, High: p + 2, Low: p - 2, Close: p, Interval: 24 * time.Hour}
	}
	cs[n-1].Close = cs[n-1].High + 30 // breakout above recent Donchian
	return cs
}

type harness struct {
	broker    *broker.Paper
	posRepo   *repository.InMemoryPositionRepo
	tradeRepo *repository.InMemoryTradeRepo
	closer    *repository.Closer
	emergency *safety.EmergencyStop
	cycle     *TradingCycle
	manage    *ManageOpenPositions
	flatten   *ForceFlatten
	hours     session.TradingHours
	now       time.Time
}

func newHarness(t *testing.T, now time.Time) *harness {
	t.Helper()
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 1, 0)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	closer := repository.NewCloser(posRepo, tradeRepo)
	pending := safety.NewPendingPositions()
	es := safety.NewEmergencyStop(testutil.TempFlagPath(t), nil)
	hours := tokyoHours()

	eng := strategy.NewEngine(func() string { return "sig-1" }, strategy.TimeSeriesMomentum{})
	caps := SnapshotCaps{
		MaxDailyLossJPY: 5000, MaxConsecutiveLosses: 4, PerSymbolMaxOpenPositions: 1,
		AccountMaxOpenPositions: 3, AccountMaxDailyLossJPY: 18000, RequiredMarginRate: 0.30, WindowMinutes: 60,
	}
	snap := NewSnapshotBuilder(posRepo, tradeRepo, pb, es, hours, c, caps)
	exec := NewExecuteOrder(pb, posRepo, pending, es, c)
	execKindFor := func(m order.HoldingMode) order.ExecKind {
		if m == order.HoldingMultiday {
			return order.ExecMarginGeneral
		}
		return order.ExecMarginOneday
	}
	cycle := NewTradingCycle(eng, snap, exec, 100, execKindFor, hours)
	manage := NewManageOpenPositions(pb, posRepo, closer, es, c, 0.30)
	flatten := NewForceFlatten(pb, posRepo, closer, es, hours, c)

	return &harness{broker: pb, posRepo: posRepo, tradeRepo: tradeRepo, closer: closer, emergency: es,
		cycle: cycle, manage: manage, flatten: flatten, hours: hours, now: now}
}

func multidayConfig() *config.StrategyConfig {
	c := &config.StrategyConfig{ConfigID: "cfg-1", Symbol: "7203", StrategyName: config.StrategyTimeSeriesMomentum, HoldingMode: order.HoldingMultiday, ExecKind: config.ExecMarginGeneral}
	c.Entry.Direction = config.DirectionBoth
	c.Entry.MaxSpreadTicks = 5
	c.Exit.TakeProfitJPY = 100
	c.Exit.StopLossJPY = 50
	c.Exit.MaxHoldMinutes = 240
	c.Risk.Quantity = 100
	c.Risk.MaxOpenPositions = 1
	return c
}

func evalInput(now time.Time, cfg *config.StrategyConfig, daily []market.Candle) strategy.EvalInput {
	last := daily[len(daily)-1].Close
	return strategy.EvalInput{
		Now: now, Config: cfg, CandlesDaily: daily,
		Summary: &market.MarketSummary{Symbol: "7203", TickSize: market.TickSize(last),
			CurrentRate: market.CurrentRate{Last: last, SpreadTicks: 1}},
	}
}

func TestTradingCycle_EntersAndExecuteSagaCreatesPosition(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	cfg := multidayConfig()
	cfg.HoldingMode = order.HoldingMultiday
	cfg.ExecKind = config.ExecMarginGeneral
	in := evalInput(now, cfg, dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	res, err := h.cycle.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if !res.Entered {
		t.Fatalf("expected entry, got reject=%q decision=%q", res.RejectReason, res.Signal.Decision)
	}
	open, _ := h.posRepo.ListOpenOrClosing(context.Background(), "7203")
	if len(open) != 1 || open[0].Status != position.StatusOpen {
		t.Fatalf("expected 1 open position, got %+v", open)
	}
	// 守りは板(broker 側)にあることが不変条件。台帳の leg id 写しは持たない。
	orders, _ := h.broker.GetActiveOrders(context.Background(), "7203")
	if len(orders) != 1 || orders[0].Side != order.SideSell {
		t.Fatalf("protective order must rest on the board close-side, got %+v", orders)
	}
}

func TestTradingCycle_RejectsOnedayHoldingMultiday(t *testing.T) {
	// Enforced on the RESOLVED exec kind, not cfg.ExecKind.
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	eng := strategy.NewEngine(func() string { return "s" }, strategy.TimeSeriesMomentum{})
	snap := NewSnapshotBuilder(posRepo, tradeRepo, pb, es, tokyoHours(), c, SnapshotCaps{RequiredMarginRate: 0.3})
	exec := NewExecuteOrder(pb, posRepo, safety.NewPendingPositions(), es, c)
	// force margin_oneday for ALL holding modes
	onedayAlways := func(order.HoldingMode) order.ExecKind { return order.ExecMarginOneday }
	cycle := NewTradingCycle(eng, snap, exec, 100, onedayAlways, tokyoHours())

	cfg := multidayConfig() // HoldingMultiday
	in := evalInput(now, cfg, dailyUptrend(250))
	pb.SetPrice("7203", in.Summary.CurrentRate.Last)
	res, _ := cycle.Execute(context.Background(), in)
	if res.Entered || res.RejectReason != "oneday_margin_cannot_hold_multiday" {
		t.Fatalf("oneday+multiday must reject, got entered=%v reason=%q", res.Entered, res.RejectReason)
	}
}

func TestTradingCycle_ClampsIntradayMaxHold(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 14, 0, 0, 0, clock.JST) // 50 min before 14:50 force-flat
	h := newHarness(t, now)
	cfg := multidayConfig()
	cfg.HoldingMode = order.HoldingIntraday
	cfg.ExecKind = config.ExecMarginOneday
	cfg.Exit.MaxHoldMinutes = 600 // absurdly long; must be clamped to <= 50
	in := evalInput(now, cfg, dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)
	res, err := h.cycle.Execute(ctx, in)
	if err != nil || !res.Entered {
		t.Fatalf("entry expected, got entered=%v reason=%q err=%v", res.Entered, res.RejectReason, err)
	}
	open, _ := h.posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 || open[0].MaxHoldMinutes > 50 || open[0].MaxHoldMinutes <= 0 {
		t.Fatalf("intraday MaxHold not clamped to force-flat window: %+v", open)
	}
}

func TestTradingCycle_EmergencyBlocksEntry(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	_ = h.emergency.Trip("test", now)
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	res, _ := h.cycle.Execute(context.Background(), in)
	if res.Entered || res.RejectReason != "emergency_stop" {
		t.Fatalf("emergency should block entry, got entered=%v reason=%q", res.Entered, res.RejectReason)
	}
}

func TestTradingCycle_RecordsRejectionToRepo(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	rejections := repository.NewInMemoryRejectionRepo()
	h.cycle.Rejections = rejections
	_ = h.emergency.Trip("test", now)
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	if _, err := h.cycle.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	rows := rejections.All()
	if len(rows) != 1 {
		t.Fatalf("expected 1 recorded rejection, got %+v", rows)
	}
	r := rows[0]
	if r.Symbol != "7203" || r.Reason != "emergency_stop" || r.ConfigID == "" || !r.CreatedAt.Equal(now) {
		t.Fatalf("rejection row incomplete: %+v", r)
	}
}

// 毎 tick 同じ理由を書き続けるとログとして機能しない(実測 233,193 行)。
func TestTradingCycle_DedupsConsecutiveSameKindRejections(t *testing.T) {
	now := time.Date(2026, 6, 17, 16, 0, 0, 0, clock.JST) // after close
	h := newHarness(t, now)
	rejections := repository.NewInMemoryRejectionRepo()
	h.cycle.Rejections = rejections
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	for i := 0; i < 3; i++ {
		if _, err := h.cycle.Execute(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	if rows := rejections.All(); len(rows) != 1 || rows[0].Reason != "outside_session_hours" {
		t.Fatalf("同一種別の連続 reject は 1 行に畳まれるべき: %+v", rows)
	}

	// emergency はゲートの評価順で session より先なので、trip で種別が切り替わる。
	_ = h.emergency.Trip("test", now)
	if _, err := h.cycle.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	rows := rejections.All()
	if len(rows) != 2 || rows[1].Reason != "emergency_stop" {
		t.Fatalf("種別変化で記録されるべき: %+v", rows)
	}
	if _, err := h.cycle.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if rows := rejections.All(); len(rows) != 2 {
		t.Fatalf("エッジ後の連続 reject が畳まれていない: %d 行", len(rows))
	}
}

// リセットが消えると entry 後の同種 reject が全て沈黙する回帰を検出する。
func TestTradingCycle_EntryResetsRejectionEdge(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	rejections := repository.NewInMemoryRejectionRepo()
	h.cycle.Rejections = rejections
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	_ = h.emergency.Trip("test", now)
	if _, err := h.cycle.Execute(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := h.emergency.Resume(); err != nil {
		t.Fatal(err)
	}
	res, err := h.cycle.Execute(ctx, in)
	if err != nil || !res.Entered {
		t.Fatalf("entry expected after resume: %+v err=%v", res, err)
	}
	_ = h.emergency.Trip("test2", now)
	if _, err := h.cycle.Execute(ctx, in); err != nil {
		t.Fatal(err)
	}
	rows := rejections.All()
	if len(rows) != 2 || rows[0].Reason != "emergency_stop" || rows[1].Reason != "emergency_stop" {
		t.Fatalf("entry 後の同種 reject が再記録されていない: %+v", rows)
	}
}

// 週またぎの建玉で月曜が無記録になる穴を塞ぐ。
func TestTradingCycle_RejectionEdgeResetsAcrossDays(t *testing.T) {
	ctx := context.Background()
	day1 := time.Date(2026, 6, 17, 16, 0, 0, 0, clock.JST) // after close
	h := newHarness(t, day1)
	rejections := repository.NewInMemoryRejectionRepo()
	h.cycle.Rejections = rejections

	in1 := evalInput(day1, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in1.Summary.CurrentRate.Last)
	for i := 0; i < 2; i++ {
		if _, err := h.cycle.Execute(ctx, in1); err != nil {
			t.Fatal(err)
		}
	}
	day2 := day1.AddDate(0, 0, 1)
	in2 := evalInput(day2, multidayConfig(), dailyUptrend(250))
	if _, err := h.cycle.Execute(ctx, in2); err != nil {
		t.Fatal(err)
	}
	rows := rejections.All()
	if len(rows) != 2 {
		t.Fatalf("日跨ぎで再記録されるべき: %+v", rows)
	}
}

// reason は GROUP BY できる安定種別だけを持ち、可変部は detail に分離する。
func TestSplitRejectReason(t *testing.T) {
	cases := []struct{ in, kind, detail string }{
		{"cooldown after_loss until 09:37:25", "cooldown", "after_loss until 09:37:25"},
		{"open_positions 1 >= cap 1", "open_positions", "1 >= cap 1"},
		{"daily_loss 5000 >= cap 4000", "daily_loss", "5000 >= cap 4000"},
		{"emergency_stop", "emergency_stop", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		kind, detail := splitRejectReason(c.in)
		if kind != c.kind || detail != c.detail {
			t.Fatalf("splitRejectReason(%q) = (%q, %q), want (%q, %q)", c.in, kind, detail, c.kind, c.detail)
		}
	}
}

func TestTradingCycle_RecordsKindAndDetail(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	rejections := repository.NewInMemoryRejectionRepo()
	h.cycle.Rejections = rejections
	sig := strategy.Signal{Symbol: "7203", ConfigID: "cfg-x"}
	h.cycle.recordRejection(context.Background(), sig, "cooldown after_loss until 09:37:25", now)
	rows := rejections.All()
	if len(rows) != 1 || rows[0].Reason != "cooldown" || rows[0].Detail != "after_loss until 09:37:25" {
		t.Fatalf("kind/detail 分離が効いていない: %+v", rows)
	}
}

func TestTradingCycle_OutsideSessionBlocksEntry(t *testing.T) {
	now := time.Date(2026, 6, 17, 16, 0, 0, 0, clock.JST) // after close
	h := newHarness(t, now)
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	res, _ := h.cycle.Execute(context.Background(), in)
	if res.Entered || res.RejectReason != "outside_session_hours" {
		t.Fatalf("outside session should block, got entered=%v reason=%q", res.Entered, res.RejectReason)
	}
}

func TestManageOpenPositions_TakeProfitCloses(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	entry := in.Summary.CurrentRate.Last
	h.broker.SetPrice("7203", entry)
	if _, err := h.cycle.Execute(ctx, in); err != nil {
		t.Fatalf("entry: %v", err)
	}

	ts := market.TickSize(entry)
	h.broker.SetPrice("7203", entry+120*ts)
	summary := market.SummaryFromTicker(market.Ticker{Symbol: "7203", Bid: entry + 119*ts, Ask: entry + 121*ts, Last: entry + 120*ts}, now)
	if err := h.manage.OnTick(ctx, "7203", summary); err != nil {
		t.Fatalf("manage: %v", err)
	}

	open, _ := h.posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 0 {
		t.Fatalf("position should be closed on TP, still open: %+v", open)
	}
	loss, _ := h.tradeRepo.CountTradesSinceBySymbol(ctx, "7203", now.Add(-time.Hour))
	if loss != 1 {
		t.Fatalf("expected 1 recorded trade, got %d", loss)
	}
}

func TestForceFlatten_ClosesIntradayNearClose(t *testing.T) {
	ctx := context.Background()
	openTime := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, openTime)

	cfg := multidayConfig()
	cfg.HoldingMode = order.HoldingIntraday
	cfg.ExecKind = config.ExecMarginOneday
	in := evalInput(openTime, cfg, dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)
	if _, err := h.cycle.Execute(ctx, in); err != nil {
		t.Fatalf("entry: %v", err)
	}

	nearClose := time.Date(2026, 6, 17, 14, 51, 0, 0, clock.JST)
	h2 := rewire(t, h, nearClose)
	summary := market.SummaryFromTicker(market.Ticker{Symbol: "7203", Last: in.Summary.CurrentRate.Last}, nearClose)
	n, err := h2.flatten.FlattenIfNearClose(ctx, "7203", summary)
	if err != nil || n != 1 {
		t.Fatalf("force flatten near close: n=%d err=%v", n, err)
	}
	open, _ := h.posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 0 {
		t.Fatalf("intraday position should be flattened, still open: %+v", open)
	}
}

// rewire rebuilds the time-dependent commands at a new clock; repos/broker (and
// therefore positions) are reused.
func rewire(t *testing.T, h *harness, now time.Time) *harness {
	t.Helper()
	c := clock.Fixed(now)
	h.flatten = NewForceFlatten(h.broker, h.posRepo, h.closer, h.emergency, h.hours, c)
	h.manage = NewManageOpenPositions(h.broker, h.posRepo, h.closer, h.emergency, c, 0.30)
	return h
}

func reconcileHarness(t *testing.T, now time.Time) (*broker.Paper, *repository.InMemoryPositionRepo, *repository.InMemoryTradeRepo, *safety.EmergencyStop, *Reconcile) {
	t.Helper()
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	closer := repository.NewCloser(posRepo, tradeRepo)
	pending := safety.NewPendingPositions()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	return pb, posRepo, tradeRepo, es, NewReconcile(pb, posRepo, closer, pending, es, c)
}

func TestReconcile_AdoptUnprotectedOrphanTrips(t *testing.T) {
	// No working close-side order = an unprotected orphan (e.g. a bot crash between
	// fill and OCO).
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb, _, _, es, rec := reconcileHarness(t, now)

	pb.SetPrice("7203", 2500)
	if _, err := pb.PlaceOrder(ctx, order.PlaceOrderRequest{Symbol: "7203", Side: order.SideBuy, Quantity: 100}); err != nil {
		t.Fatal(err)
	}
	rep, err := rec.Run(ctx, "7203")
	if err != nil || rep.Adopted != 1 {
		t.Fatalf("expected 1 adoption, got %+v err=%v", rep, err)
	}
	if !es.Active() || rep.Tripped != 1 {
		t.Fatalf("unprotected orphan must trip emergency, got active=%v rep=%+v (reason=%q)", es.Active(), rep, es.Reason())
	}
}

func TestReconcile_AdoptProtectedPositionDoesNotTrip(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb, _, _, es, rec := reconcileHarness(t, now)

	pb.SetPrice("7203", 2500)
	placed, err := pb.PlaceOrder(ctx, order.PlaceOrderRequest{Symbol: "7203", Side: order.SideBuy, Quantity: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pb.PlaceSettleOCO(ctx, port.OCOCloseOrderInput{
		Symbol: "7203", BrokerPositionID: placed.BrokerPositionID, Side: order.SideSell,
		Quantity: 100, TakeProfit: 2600, StopLoss: 2400,
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := rec.Run(ctx, "7203")
	if err != nil || rep.Adopted != 1 {
		t.Fatalf("expected 1 adoption, got %+v err=%v", rep, err)
	}
	if es.Active() {
		t.Fatalf("protected adoption must NOT trip, reason=%q", es.Reason())
	}
}

func TestReconcile_StaleDefersThenColdCloses(t *testing.T) {
	// Not left to rot until the 10-minute trip.
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb, posRepo, tradeRepo, es, rec := reconcileHarness(t, now)

	pb.SetPrice("6758", 3000)
	id, _ := posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "ghost-1", Symbol: "6758", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 2900, OpenedAt: now,
	})
	rep, _ := rec.Run(ctx, "6758")
	if rep.Deferred != 1 || rep.ColdClosed != 0 {
		t.Fatalf("within grace: expected defer, got %+v", rep)
	}

	rec.clock = clock.Fixed(now.Add(2 * time.Minute))
	rep2, _ := rec.Run(ctx, "6758")
	if rep2.ColdClosed != 1 {
		t.Fatalf("past grace: expected cold close, got %+v", rep2)
	}
	open, _ := posRepo.ListOpenOrClosing(ctx, "6758")
	if len(open) != 0 {
		t.Fatalf("position should be CLOSED after cold close, got %+v", open)
	}
	n, _ := tradeRepo.CountTradesSinceBySymbol(ctx, "6758", now.Add(-time.Hour))
	if n != 1 {
		t.Fatalf("cold close must record exactly 1 trade, got %d", n)
	}
	if es.Active() {
		t.Fatalf("cold close must resolve without a trip, reason=%q", es.Reason())
	}
	_ = id
}

func TestReconcile_StuckClosingRetriesClose(t *testing.T) {
	// A CLOSING position the broker STILL holds would otherwise be orphaned forever.
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb, posRepo, _, es, rec := reconcileHarness(t, now)

	pb.SetPrice("7203", 2500)
	placed, _ := pb.PlaceOrder(ctx, order.PlaceOrderRequest{Symbol: "7203", Side: order.SideBuy, Quantity: 100})
	id, _ := posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: placed.BrokerPositionID, Symbol: "7203", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 2500, OpenedAt: now,
	})
	if ok, _ := posRepo.ClaimForClose(ctx, id, now); !ok {
		t.Fatal("claim failed")
	}
	rep, _ := rec.Run(ctx, "7203")
	if rep.Deferred != 1 {
		t.Fatalf("within grace: expected defer, got %+v", rep)
	}
	rec.clock = clock.Fixed(now.Add(2 * time.Minute))
	rep2, _ := rec.Run(ctx, "7203")
	if rep2.ColdClosed != 1 {
		t.Fatalf("past grace: expected retried close, got %+v", rep2)
	}
	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 0 {
		t.Fatalf("stuck CLOSING should be closed, got %+v", open)
	}
	if bps, _ := pb.GetPositions(ctx); len(bps) != 0 {
		t.Fatalf("broker should hold nothing after the retried close, got %+v", bps)
	}
	if es.Active() {
		t.Fatalf("retry must resolve without a trip, reason=%q", es.Reason())
	}
}

func TestForceFlatten_ExternalOnedayIsFlattenedOthersAreNot(t *testing.T) {
	// The carry-over penalty is unconditional, so an adopted 一日信用 position must
	// still be flattened; other external positions stay display-only.
	ctx := context.Background()
	nearClose := time.Date(2026, 6, 17, 14, 51, 0, 0, clock.JST)
	h := newHarness(t, nearClose)

	h.broker.SetPrice("7203", 2500)
	placed, _ := h.broker.PlaceOrder(ctx, order.PlaceOrderRequest{Symbol: "7203", Side: order.SideBuy, Quantity: 100})
	if _, err := h.posRepo.AdoptExternal(ctx, port.BrokerPosition{
		BrokerPositionID: placed.BrokerPositionID, Symbol: "7203", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 2500, ExecKind: order.ExecMarginOneday,
	}, nearClose); err != nil {
		t.Fatal(err)
	}
	if _, err := h.posRepo.AdoptExternal(ctx, port.BrokerPosition{
		BrokerPositionID: "human-1", Symbol: "7203", Side: order.SideSell,
		Quantity: 200, EntryPrice: 2600, ExecKind: order.ExecMarginGeneral,
	}, nearClose); err != nil {
		t.Fatal(err)
	}

	summary := market.SummaryFromTicker(market.Ticker{Symbol: "7203", Last: 2500}, nearClose)
	n, err := h.flatten.FlattenIfNearClose(ctx, "7203", summary)
	if err != nil || n != 1 {
		t.Fatalf("expected exactly the oneday external flattened, n=%d err=%v", n, err)
	}
	open, _ := h.posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 || open[0].BrokerPositionID != "human-1" {
		t.Fatalf("the human's 一般信用 position must remain, got %+v", open)
	}
}

func TestReconcile_ExternalVanishedIsMarkedClosed(t *testing.T) {
	// Staying OPEN would block the nanpin gate for that symbol forever.
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb, posRepo, _, _, rec := reconcileHarness(t, now)
	pb.SetPrice("7203", 2500)
	if _, err := posRepo.AdoptExternal(ctx, port.BrokerPosition{
		BrokerPositionID: "ext-1", Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 2500,
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Run(ctx, "7203"); err != nil {
		t.Fatal(err)
	}
	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 0 {
		t.Fatalf("vanished external should be marked closed, got %+v", open)
	}
}

// 守りの注文期日: 多日保有だけ延ばす。
//   - intraday は 14:50 に強制フラット化されるので当日限りが正しい(zero)。
//     翌日まで残る決済注文の方が危険 — 建玉が無いのに売り注文が板に立つ。
//   - multiday は立花の上限 10 営業日の**内側**に置く。仕様は「指定した日を含む
//     10営業日迄」で、含む/含まないの解釈が曖昧なので 1 日余らせる。
//   - 休場カレンダーが尽きたら zero。日付を捏造して休場日を指定すると注文ごと
//     拒否される。zero を受けた TradingCycle は multiday を**建てない**
//     (TestTradingCycle_RejectsMultidayEntryWhenProtectiveExpiryUnavailable)。
func TestSettleOrderExpiry(t *testing.T) {
	hours := tokyoHours()
	thu := time.Date(2026, 8, 13, 12, 45, 0, 0, clock.JST)

	if got := settleOrderExpiry(hours, order.HoldingIntraday, thu); !got.IsZero() {
		t.Errorf("intraday で期日 %v — 当日限りであるべき", got)
	}
	got := settleOrderExpiry(hours, order.HoldingMultiday, thu)
	if got.IsZero() {
		t.Fatal("multiday なのに当日限り — 2日目に守りが消える")
	}
	if !got.After(thu) {
		t.Errorf("期日 %v が当日以前", got)
	}
	// 上限の内側であること(営業日で数えて 10 未満)。
	n := 0
	for d := thu; d.Before(got); d = d.AddDate(0, 0, 1) {
		if hours.IsTradingDay(d) {
			n++
		}
	}
	if n >= 10 {
		t.Errorf("期日まで %d 営業日 — 立花の上限 10 営業日を超える/接する", n)
	}
}

// 🚨 **休場カレンダーが尽きていて期日を出せないなら、多日建玉は建てない**。
// 従来は当日限りの守りで建てていた = 翌日から裸で、しかも
// Arm / Reprice / Replace は同じ状態で error を返すので誰も直せない。建玉が無ければ
// 守るものも無い。intraday は 14:50 に強制フラット化されるので従来どおり建てる。
func TestTradingCycle_RejectsMultidayEntryWhenProtectiveExpiryUnavailable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	build := func(t *testing.T, hours session.TradingHours) (*TradingCycle, *broker.Paper) {
		t.Helper()
		c := clock.Fixed(now)
		pb := broker.NewPaper(c, 1, 0)
		posRepo := repository.NewInMemoryPositionRepo()
		es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
		eng := strategy.NewEngine(func() string { return "s" }, strategy.TimeSeriesMomentum{})
		snap := NewSnapshotBuilder(posRepo, repository.NewInMemoryTradeRepo(), pb, es, hours, c, SnapshotCaps{RequiredMarginRate: 0.3})
		exec := NewExecuteOrder(pb, posRepo, safety.NewPendingPositions(), es, c)
		generalAlways := func(order.HoldingMode) order.ExecKind { return order.ExecMarginGeneral }
		return NewTradingCycle(eng, snap, exec, 100, generalAlways, hours), pb
	}

	// 9 営業日先が calendar_through を超える(6/17 から見て 6/23 まで 4 営業日しか無い)。
	short := tokyoHours()
	short.CalendarThrough = time.Date(2026, 6, 23, 0, 0, 0, 0, clock.JST)
	cycle, pb := build(t, short)
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	pb.SetPrice("7203", in.Summary.CurrentRate.Last)
	res, err := cycle.Execute(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Entered || res.RejectReason != "protective_expiry_unavailable_calendar_exhausted" {
		t.Fatalf("期日が出せないのに建てた: entered=%v reason=%q", res.Entered, res.RejectReason)
	}

	// 対照: カレンダーが足りていれば同じ入力で建つ。
	cycle, pb = build(t, tokyoHours())
	pb.SetPrice("7203", in.Summary.CurrentRate.Last)
	res, err = cycle.Execute(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Entered {
		t.Fatalf("対照が建たない(reject の理由がカレンダーでない): reason=%q", res.RejectReason)
	}
}

// 🛑 ポーリング間隔が猶予窓より長いと、cold close の枝に**到達できない**。
//
// live の reconcile は口座照会の回数を絞るため建玉保有中でも1時間おき。
// 段階判定を経過時間だけで書いていたので、2回目の観測で age が 1 時間になり、
// 90秒〜10分の cold close 窓を飛び越えて hardPeriod(10分)の緊急停止に入っていた。
// つまり **broker 側で TP/SL が約定するたびに、決済が台帳に残らないまま緊急停止**する。
// 決済 → 台帳の最後の 1 歩が構造的に通らない状態だった。
//
// 直し方は窓を広げることではない(間隔を変えるたびに壊れる)。**先に解決を試み、
// 解決できなかったときだけ trip する**。trip は「未解決」に対する手段であって、
// 時間の経過そのものに対する手段ではない。
func TestReconcile_LongPollIntervalStillColdCloses(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb, posRepo, tradeRepo, es, rec := reconcileHarness(t, now)

	pb.SetPrice("6758", 3000)
	if _, err := posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "ghost-2", Symbol: "6758", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 2900, OpenedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if rep, _ := rec.Run(ctx, "6758"); rep.Deferred != 1 {
		t.Fatalf("初回観測は待機のはず: %+v", rep)
	}

	// 次の観測は 1 時間後(live の実際の間隔)。
	rec.clock = clock.Fixed(now.Add(time.Hour))
	rep, _ := rec.Run(ctx, "6758")
	if rep.ColdClosed != 1 {
		t.Fatalf("1時間間隔でも cold close されるべき: %+v", rep)
	}
	if rep.Tripped != 0 || es.Active() {
		t.Fatalf("解決できたのに緊急停止した (reason=%q): %+v", es.Reason(), rep)
	}
	n, _ := tradeRepo.CountTradesSinceBySymbol(ctx, "6758", now.Add(-time.Hour))
	if n != 1 {
		t.Fatalf("決済が台帳に残っていない: trades=%d", n)
	}
}

// ─── live 事故(同一銘柄で買い→売りを5往復)の回帰 ─────────────────
// 守りの価格が当日の値幅制限の外に出る建玉は、約定した後に broker が守りの発注を
// 拒否する。SL が帯の外なら**建てる前に**見送る(建ててからでは補償で巻き戻すしかない)。
// wideStopStrategy は「出口幅が config そのまま」の最小戦略。TimeSeriesMomentum は
// 自前で 10〜15 円の出口を作るので、値幅制限(最小でも ±30 円)の外に出せない。
// MOCK rationale (TESTING.md 3用途): §3 テスト専用の最小実装(本番戦略ではない)。
type wideStopStrategy struct{}

func (wideStopStrategy) Name() config.StrategyName { return config.StrategyTimeSeriesMomentum }

func (wideStopStrategy) Evaluate(in strategy.EvalInput) strategy.Signal {
	last := in.CandlesDaily[len(in.CandlesDaily)-1].Close
	return strategy.Signal{
		Decision: strategy.DecisionEnter, SignalID: "sig-w", Symbol: in.Config.Symbol,
		Side: order.SideBuy, EntryPrice: last, Quantity: in.Config.Risk.Quantity,
		TakeProfitJPY: in.Config.Exit.TakeProfitJPY, StopLossJPY: in.Config.Exit.StopLossJPY,
		MaxHoldMinutes: in.Config.Exit.MaxHoldMinutes, HoldingMode: in.Config.HoldingMode,
		ConfigID: in.Config.ConfigID, StrategyName: in.Config.StrategyName, CreatedAt: in.Now,
	}
}

func TestTradingCycle_RejectsEntryWhenStopIsOutsidePriceLimit(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	h.cycle.engine = strategy.NewEngine(func() string { return "sig-w" }, wideStopStrategy{})
	cfg := multidayConfig()
	// 前日終値 2,779 → 帯は ±500(2,000〜3,000 未満)= 2,279〜3,279。SL 幅 700 は下限の外。
	cfg.Exit.StopLossJPY = 700
	cfg.Exit.TakeProfitJPY = 100
	rejections := repository.NewInMemoryRejectionRepo()
	h.cycle.Rejections = rejections
	in := evalInput(now, cfg, dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	res, err := h.cycle.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if res.Entered {
		t.Fatal("SL が値幅制限の外なのに建てた — 約定後に守りを置けず巻き戻すことになる")
	}
	if res.RejectReason != "stop_loss_outside_price_limit" {
		t.Fatalf("RejectReason = %q, want stop_loss_outside_price_limit", res.RejectReason)
	}
	// 見送りは台帳に残す(「何も起きなかった日」に見せない)。
	if len(rejections.All()) == 0 {
		t.Fatal("見送りが signal_rejections に記録されていない")
	}
}

// 帯の内側の SL は従来どおり通る(このゲートが普通のエントリーを塞がないこと)。
func TestTradingCycle_AllowsEntryWhenStopIsInsidePriceLimit(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	h.cycle.engine = strategy.NewEngine(func() string { return "sig-w" }, wideStopStrategy{})
	cfg := multidayConfig() // SL 幅 50 → 帯 ±500 の内側
	in := evalInput(now, cfg, dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	res, err := h.cycle.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if !res.Entered {
		t.Fatalf("帯の内側なのに見送った: %q", res.RejectReason)
	}
}

// 補償で巻き戻した銘柄は、その日はもう建てない。台帳側のゲート(cooldown / 窓の
// 取引回数 / 日次損失 / 連敗)は台帳を読むので、台帳が書けなかった枝でも止まるように
// executor 側の記憶をゲートとして見る。
func TestTradingCycle_RejectsSymbolBlockedByEarlierRollback(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	cfg := multidayConfig()
	in := evalInput(now, cfg, dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)
	h.cycle.executor.blockSymbolForDay("7203")

	res, err := h.cycle.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if res.Entered {
		t.Fatal("同日に巻き戻した銘柄をまた建てた — これが 5 往復の形")
	}
	if res.RejectReason != "entry_rolled_back_today" {
		t.Fatalf("RejectReason = %q, want entry_rolled_back_today", res.RejectReason)
	}
}

// 🛑 external(人間が証券アプリで建てた)建玉が broker から消えたとき、これまでは
// MarkClosed するだけで **trade 行を作らなかった**。「その PnL は bot のものではない」
// のは戦略成績としては正しいが、**口座には実額として効いている**(委託保証金・維持率・
// 日次損失)。台帳から永久に消えるのは監査上おかしい。戦略の出口ではないので
// close_reason で区別し、エッジ判定からは別途外す。
func TestReconcile_ExternalCloseIsBookedInTheLedger(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb, posRepo, tradeRepo, _, rec := reconcileHarness(t, now)
	pb.SetPrice("7203", 2600)

	// 人間の建玉として台帳にだけ存在し、broker には**もう無い**状態。
	id, err := posRepo.AdoptExternal(ctx, port.BrokerPosition{
		BrokerPositionID: "shinyo:7203", Symbol: "7203", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 2500, ExecKind: order.ExecMarginGeneral,
	}, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}

	if _, err := rec.Run(ctx, "7203"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	trades, _ := tradeRepo.ListClosedSince(ctx, now.Add(-time.Hour))
	if len(trades) != 1 {
		t.Fatalf("trade = %d 件, want 1 — external の決済が台帳から消えている", len(trades))
	}
	if trades[0].CloseReason != "external_close" {
		t.Fatalf("close_reason = %q, want external_close(戦略の出口と区別できないとエッジ台帳が汚れる)", trades[0].CloseReason)
	}
	if trades[0].ProfitLossJPY != 10000 { // (2600-2500) * 100
		t.Fatalf("gross = %v, want 10000", trades[0].ProfitLossJPY)
	}
	if !trades[0].FeeEstimated {
		t.Fatal("手数料は同期に取れないので estimated で立てること")
	}
	p, _ := posRepo.GetByID(ctx, id)
	if p == nil || p.Status != position.StatusClosed {
		t.Fatalf("建玉が CLOSED になっていない: %+v", p)
	}
}

// 価格が観測できないときは **trade を書かない**(PnL 捏造禁止)。ただし建玉は閉じる —
// OPEN のまま残すとその銘柄のナンピン禁止が永久に閉じる(従来の挙動を保つ)。
func TestReconcile_ExternalCloseWithoutPriceStillClosesButBooksNothing(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb, posRepo, tradeRepo, _, rec := reconcileHarness(t, now)
	_ = pb // 価格を入れない = 観測不能

	id, err := posRepo.AdoptExternal(ctx, port.BrokerPosition{
		BrokerPositionID: "shinyo:9999", Symbol: "9999", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 2500, ExecKind: order.ExecMarginGeneral,
	}, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}

	if _, err := rec.Run(ctx, "9999"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if trades, _ := tradeRepo.ListClosedSince(ctx, now.Add(-time.Hour)); len(trades) != 0 {
		t.Fatalf("価格不明なのに trade を書いた: %+v", trades)
	}
	p, _ := posRepo.GetByID(ctx, id)
	if p == nil || p.Status != position.StatusClosed {
		t.Fatalf("建玉は閉じること(OPEN のままだとナンピン禁止が永久に閉じる): %+v", p)
	}
}

// 🚨 建値ゼロの external 建玉で **巨額の偽 PnL** を台帳に書かない(レビュー指摘)。
// 立花が建値を返さない/取れない建玉を adopt すると EntryPrice=0 で凍結され、決済時に
// grossPnL は (現値 − 0) × 数量 = 現値ぶん丸ごとを「利益」として書いてしまう。
// 日次損失 cap・維持率の判断材料になる数字なので、確かめられない値は書かない。
func TestReconcile_ExternalCloseWithZeroEntryPriceBooksNothing(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb, posRepo, tradeRepo, _, rec := reconcileHarness(t, now)
	pb.SetPrice("7203", 2600)

	id, err := posRepo.AdoptExternal(ctx, port.BrokerPosition{
		BrokerPositionID: "shinyo:7203", Symbol: "7203", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 0, ExecKind: order.ExecMarginGeneral, // 建値不明
	}, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}

	if _, err := rec.Run(ctx, "7203"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if trades, _ := tradeRepo.ListClosedSince(ctx, now.Add(-time.Hour)); len(trades) != 0 {
		t.Fatalf("建値ゼロなのに trade を書いた(偽 PnL): %+v", trades)
	}
	p, _ := posRepo.GetByID(ctx, id)
	if p == nil || p.Status != position.StatusClosed {
		t.Fatalf("建玉は閉じること: %+v", p)
	}
}

// tpOnlyBoard は「決済側の注文はあるが逆指値脚が無い」板。人間が利確指値だけを
// 置いた建玉や、OCO の SL 脚だけが失効した建玉がこの形になる。
type tpOnlyBoard struct {
	*broker.Paper
}

func (b tpOnlyBoard) GetActiveOrders(ctx context.Context, symbol string) ([]order.Order, error) {
	out, err := b.Paper.GetActiveOrders(ctx, symbol)
	if err != nil {
		return nil, err
	}
	return append(out, order.Order{
		OrderID: "tp-only", Symbol: symbol, Side: order.SideSell,
		Quantity: 100, Price: 2600, Status: "WORKING", // HasStopLeg=false: SL はどこにも無い
	}), nil
}

// 🛑 利確指値だけの建玉は**裸**。決済側の注文があることを守りと数えると、SL の
// 無い実弾建玉が external_adopt_unprotected を素通りする。
func TestReconcile_AdoptTakeProfitOnlyPositionTrips(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	posRepo := repository.NewInMemoryPositionRepo()
	closer := repository.NewCloser(posRepo, repository.NewInMemoryTradeRepo())
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	rec := NewReconcile(tpOnlyBoard{pb}, posRepo, closer, safety.NewPendingPositions(), es, c)

	pb.SetPrice("7203", 2500)
	if _, err := pb.PlaceOrder(ctx, order.PlaceOrderRequest{Symbol: "7203", Side: order.SideBuy, Quantity: 100}); err != nil {
		t.Fatal(err)
	}
	rep, err := rec.Run(ctx, "7203")
	if err != nil || rep.Adopted != 1 {
		t.Fatalf("expected 1 adoption, got %+v err=%v", rep, err)
	}
	if !es.Active() || rep.Tripped != 1 {
		t.Fatalf("利確指値だけの建玉は裸なので trip しなければならない: active=%v rep=%+v reason=%q", es.Active(), rep, es.Reason())
	}
}
