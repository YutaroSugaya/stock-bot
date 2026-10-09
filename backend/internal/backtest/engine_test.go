package backtest

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/strategy"
)

// entryAt2500 enters only when the last price is exactly 2500, so entries land
// on "flat" bars and never on a resolution (win/loss) bar.
type entryAt2500 struct {
	tp, sl float64
	qty    int
	mode   order.HoldingMode
}

func (entryAt2500) Name() config.StrategyName { return config.StrategyName("entry_at_2500") }
func (s entryAt2500) Evaluate(in strategy.EvalInput) strategy.Signal {
	if in.Summary.CurrentRate.Last != 2500 {
		return strategy.Signal{Decision: strategy.DecisionNone}
	}
	return strategy.Signal{
		Decision: strategy.DecisionEnter, Symbol: in.Config.Symbol, Side: order.SideBuy,
		EntryPrice: 2500, TakeProfitJPY: s.tp, StopLossJPY: s.sl,
		Quantity: s.qty, HoldingMode: s.mode, ConfigID: in.Config.ConfigID, CreatedAt: in.Now,
	}
}

func cfgFor(sym string) *config.StrategyConfig {
	// StrategyName must match the strategy's Name(): the backtest engine dispatches
	// through strategy.Engine exactly like production.
	c := &config.StrategyConfig{ConfigID: "bt", Symbol: sym, StrategyName: config.StrategyName("entry_at_2500"), HoldingMode: order.HoldingIntraday}
	c.Entry.Direction = config.DirectionBoth
	c.Risk.Quantity = 100
	c.Risk.MaxOpenPositions = 1
	return c
}

func bar(base time.Time, min int, o, h, l, cl float64) market.Candle {
	return market.Candle{Symbol: "X", Interval: time.Minute, OpenTime: base.Add(time.Duration(min) * time.Minute), Open: o, High: h, Low: l, Close: cl}
}

func TestReplay_KnownEdge_NetPFAboveOne(t *testing.T) {
	base := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	min := 0
	for i := 0; i < 120; i++ {
		cs = append(cs, bar(base, min, 2500, 2500, 2500, 2500))
		min++
		if i%5 == 0 {
			cs = append(cs, bar(base, min, 2500, 2500, 2489, 2495)) // loss; close != 2500 so no re-entry
		} else {
			cs = append(cs, bar(base, min, 2500, 2511, 2500, 2505)) // win; close != 2500 so no re-entry
		}
		min++
	}
	eng := NewEngine(cfgFor("X"), entryAt2500{tp: 10, sl: 10, qty: 100, mode: order.HoldingIntraday},
		CostModel{FeeRatePct: 0, SlippageTicks: 0, Spread: ConstSpread(0)}, PessimisticSLFirst)
	res, err := eng.Replay(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.N < 50 {
		t.Fatalf("too few trades: %d", res.Metrics.N)
	}
	if res.Metrics.NetPF <= 1.0 || res.Metrics.NetExpectancy <= 0 {
		t.Fatalf("expected positive edge: NetPF=%.3f exp=%.2f", res.Metrics.NetPF, res.Metrics.NetExpectancy)
	}
}

func TestReplay_CostFloorEatsThinEdge(t *testing.T) {
	base := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	min := 0
	for i := 0; i < 200; i++ {
		cs = append(cs, bar(base, min, 2500, 2500, 2500, 2500))
		min++
		if i%2 == 0 {
			cs = append(cs, bar(base, min, 2500, 2503, 2500, 2501)) // win +2 (TP 2 -> 2502)
		} else {
			cs = append(cs, bar(base, min, 2500, 2500, 2497, 2499)) // loss -2 (SL 2 -> 2498)
		}
		min++
	}
	strat := entryAt2500{tp: 2, sl: 2, qty: 100, mode: order.HoldingIntraday}
	gross := mustReplay(t, cs, strat, CostModel{FeeRatePct: 0, SlippageTicks: 0, Spread: ConstSpread(0)})
	withCost := mustReplay(t, cs, strat, CostModel{FeeRatePct: 0.05, SlippageTicks: 0.5, Spread: ConstSpread(1)})

	if withCost.Metrics.NetExpectancy >= gross.Metrics.NetExpectancy {
		t.Fatalf("cost model must reduce net expectancy: gross=%.3f withCost=%.3f", gross.Metrics.NetExpectancy, withCost.Metrics.NetExpectancy)
	}
	if withCost.Metrics.NetExpectancy >= 0 {
		t.Fatalf("cost floor should make a 50/50 edge negative, got %.3f", withCost.Metrics.NetExpectancy)
	}
}

// GROSS must be cost-model-independent (raw prices), so net-vs-gross isolates
// the full cost floor.
func TestReplay_GrossIsCostIndependent(t *testing.T) {
	base := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	min := 0
	for i := 0; i < 60; i++ {
		cs = append(cs, bar(base, min, 2500, 2500, 2500, 2500))
		min++
		if i%3 == 0 {
			cs = append(cs, bar(base, min, 2500, 2500, 2489, 2495))
		} else {
			cs = append(cs, bar(base, min, 2500, 2511, 2500, 2505))
		}
		min++
	}
	strat := entryAt2500{tp: 10, sl: 10, qty: 100, mode: order.HoldingIntraday}
	zero := mustReplay(t, cs, strat, CostModel{FeeRatePct: 0, SlippageTicks: 0, Spread: ConstSpread(0)})
	withCost := mustReplay(t, cs, strat, CostModel{FeeRatePct: 0.05, SlippageTicks: 1, Spread: ConstSpread(2)})

	if zero.Metrics.GrossExpectancy != withCost.Metrics.GrossExpectancy {
		t.Fatalf("GrossExpectancy must be cost-independent: zero=%.3f withCost=%.3f",
			zero.Metrics.GrossExpectancy, withCost.Metrics.GrossExpectancy)
	}
	if withCost.Metrics.NetExpectancy >= withCost.Metrics.GrossExpectancy {
		t.Fatalf("net must be below gross once costs apply: net=%.3f gross=%.3f",
			withCost.Metrics.NetExpectancy, withCost.Metrics.GrossExpectancy)
	}
}

func TestReplay_ConflictPolicy(t *testing.T) {
	base := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	cs := []market.Candle{
		bar(base, 0, 2500, 2500, 2500, 2500),
		bar(base, 1, 2500, 2512, 2488, 2505), // straddle: TP(2510) AND SL(2490)
		bar(base, 2, 2505, 2505, 2505, 2505),
	}
	strat := entryAt2500{tp: 10, sl: 10, qty: 100, mode: order.HoldingIntraday}
	zero := CostModel{FeeRatePct: 0, SlippageTicks: 0, Spread: ConstSpread(0)}

	pess := mustReplayPolicy(t, cs, strat, zero, PessimisticSLFirst)
	if len(pess.Trades) == 0 || pess.Trades[0].CloseReason != "stop_loss" {
		t.Fatalf("pessimistic should be stop_loss, got %+v", pess.Trades)
	}
	opt := mustReplayPolicy(t, cs, strat, zero, OptimisticTPFirst)
	if len(opt.Trades) == 0 || opt.Trades[0].CloseReason != "take_profit" {
		t.Fatalf("optimistic should be take_profit, got %+v", opt.Trades)
	}
	skip := mustReplayPolicy(t, cs, strat, zero, SkipAmbiguous)
	if skip.AmbiguousBars != 1 {
		t.Fatalf("skip should count 1 ambiguous bar, got %d", skip.AmbiguousBars)
	}
}

// A bar that both triggers entry and would hit TP intrabar must NOT close on
// that same bar (no look-ahead); it closes on a later bar.
func TestReplay_NoSameBarLookAhead(t *testing.T) {
	base := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	cs := []market.Candle{
		bar(base, 0, 2500, 2600, 2500, 2500), // entry bar; High 2600 would hit any TP intrabar
		bar(base, 1, 2501, 2501, 2501, 2501), // flat-ish, no TP/SL, no new entry (close!=2500)
	}
	res := mustReplay(t, cs, entryAt2500{tp: 5, sl: 5, qty: 100, mode: order.HoldingIntraday}, CostModel{Spread: ConstSpread(0)})
	for _, tr := range res.Trades {
		if tr.CloseReason == "take_profit" || tr.CloseReason == "stop_loss" {
			if !tr.ClosedAt.After(tr.OpenedAt) {
				t.Fatalf("TP/SL closed same bar as open (look-ahead): %+v", tr)
			}
		}
	}
}

func TestReplay_FillSignCorrectness(t *testing.T) {
	base := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	cs := []market.Candle{
		bar(base, 0, 2500, 2500, 2500, 2500),
		bar(base, 1, 2500, 2515, 2500, 2505), // TP(2510) hit; close != 2500 so no re-entry
	}
	res := mustReplay(t, cs, entryAt2500{tp: 10, sl: 10, qty: 100, mode: order.HoldingIntraday},
		CostModel{FeeRatePct: 0, SlippageTicks: 0, Spread: ConstSpread(0)})
	if len(res.Trades) != 1 {
		t.Fatalf("want exactly 1 trade, got %d: %+v", len(res.Trades), res.Trades)
	}
	tr := res.Trades[0]
	if tr.EntryPrice != 2500 || tr.ExitPrice != 2510 || tr.GrossJPY != 1000 || tr.NetJPY != 1000 || tr.CloseReason != "take_profit" {
		t.Fatalf("fill/sign wrong: %+v", tr)
	}
}

// The entry fills at the NEXT bar's OPEN, never the signal bar's own close, which
// the live path can never achieve.
func TestReplay_EntryFillsNextBarOpen(t *testing.T) {
	base := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	cs := []market.Candle{
		bar(base, 0, 2500, 2500, 2500, 2500), // signal fires (Last == 2500)
		bar(base, 1, 2530, 2540, 2525, 2535), // opens 2530 != signal close; TP(2540) hit
	}
	res := mustReplay(t, cs, entryAt2500{tp: 10, sl: 10, qty: 100, mode: order.HoldingIntraday},
		CostModel{FeeRatePct: 0, SlippageTicks: 0, Spread: ConstSpread(0)})
	if len(res.Trades) != 1 {
		t.Fatalf("want 1 trade, got %d: %+v", len(res.Trades), res.Trades)
	}
	if res.Trades[0].EntryPrice != 2530 {
		t.Fatalf("entry must fill at the NEXT bar open 2530 (no same-bar-close fill), got %.0f", res.Trades[0].EntryPrice)
	}
}

func TestReplay_SignalOnLastBarDoesNotFill(t *testing.T) {
	base := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	cs := []market.Candle{
		bar(base, 0, 2400, 2400, 2400, 2400),
		bar(base, 1, 2500, 2500, 2500, 2500), // signal on the LAST bar → cannot fill
	}
	res := mustReplay(t, cs, entryAt2500{tp: 10, sl: 10, qty: 100, mode: order.HoldingIntraday},
		CostModel{Spread: ConstSpread(0)})
	if len(res.Trades) != 0 {
		t.Fatalf("a signal on the last bar must not produce a fillable trade, got %+v", res.Trades)
	}
}

// A stop the bar GAPS THROUGH fills at the worse open, not the stop level, so
// crash losses are not understated.
func TestReplay_GapThroughStopFillsAtOpen(t *testing.T) {
	base := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	cs := []market.Candle{
		bar(base, 0, 2500, 2500, 2500, 2500),
		bar(base, 1, 2500, 2500, 2500, 2500), // fill at 2500; flat, no exit
		bar(base, 2, 2480, 2485, 2475, 2478), // GAP DOWN: opens 2480 (< SL 2490)
	}
	res := mustReplay(t, cs, entryAt2500{tp: 10, sl: 10, qty: 100, mode: order.HoldingIntraday},
		CostModel{FeeRatePct: 0, SlippageTicks: 0, Spread: ConstSpread(0)})
	if len(res.Trades) != 1 {
		t.Fatalf("want 1 trade, got %d: %+v", len(res.Trades), res.Trades)
	}
	tr := res.Trades[0]
	if tr.CloseReason != "stop_loss" || tr.ExitPrice != 2480 {
		t.Fatalf("gapped stop must fill at the worse open 2480, got reason=%s exit=%.0f", tr.CloseReason, tr.ExitPrice)
	}
	if tr.GrossJPY != -2000 { // (2480-2500)*100, not the -1000 a stop-level fill would show
		t.Fatalf("gapped loss must be booked at the open (-2000), got %.0f", tr.GrossJPY)
	}
}

// 執行区分は建玉時に engine が凍結する(本番の ExecKindFor と同じ)。それが carry を決める。
func TestReplay_CarryUsesTheFrozenExecKind(t *testing.T) {
	base := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	cs := []market.Candle{
		bar(base, 0, 2500, 2500, 2500, 2500),       // 建てる
		bar(base, 3*24*60, 2500, 2511, 2500, 2505), // 3 日後に TP
	}
	strat := entryAt2500{tp: 10, sl: 10, qty: 100, mode: order.HoldingMultiday}
	cost := CostModel{Spread: ConstSpread(0), Carry: prodCarry()}

	eng := NewEngine(cfgFor("X"), strat, cost, PessimisticSLFirst)
	eng.ExecKindFor = func(m order.HoldingMode) order.ExecKind {
		if m == order.HoldingMultiday {
			return order.ExecMarginSystem
		}
		return order.ExecCash
	}
	res, err := eng.Replay(context.Background(), cs)
	if err != nil || len(res.Trades) != 1 {
		t.Fatalf("trades = %+v, %v", res.Trades, err)
	}
	tr := res.Trades[0]
	if tr.CarryJPY >= 0 {
		t.Fatalf("信用の多日建玉に carry が付いていない: %v", tr.CarryJPY)
	}
	if tr.NetJPY != tr.GrossJPY-tr.FeeJPY+tr.CarryJPY {
		t.Fatalf("net = %v, want gross − fee + carry(%v − %v + %v)", tr.NetJPY, tr.GrossJPY, tr.FeeJPY, tr.CarryJPY)
	}

	// 執行区分を渡さない構成は carry 0(料率を捏造しない)。
	res0 := mustReplay(t, cs, strat, cost)
	if len(res0.Trades) != 1 || res0.Trades[0].CarryJPY != 0 {
		t.Fatalf("ExecKindFor なしで carry が付いた: %+v", res0.Trades)
	}
}

func mustReplay(t *testing.T, cs []market.Candle, strat strategy.Strategy, cost CostModel) Result {
	t.Helper()
	return mustReplayPolicy(t, cs, strat, cost, PessimisticSLFirst)
}

func mustReplayPolicy(t *testing.T, cs []market.Candle, strat strategy.Strategy, cost CostModel, p ConflictPolicy) Result {
	t.Helper()
	eng := NewEngine(cfgFor("X"), strat, cost, p)
	res, err := eng.Replay(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestPositionFromSignal_Freezes(t *testing.T) {
	sig := strategy.Signal{Symbol: "X", Side: order.SideBuy, TakeProfitJPY: 10, StopLossJPY: 5, HoldingMode: order.HoldingMultiday}
	p := positionFromSignal(sig, 2500, 100, time.Now())
	if p.TickSizeAtEntry != market.TickSize(2500) || p.Status != position.StatusOpen {
		t.Fatalf("freeze wrong: %+v", p)
	}
}
