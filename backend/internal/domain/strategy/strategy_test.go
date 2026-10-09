package strategy

import (
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

func baseConfig() *config.StrategyConfig {
	c := &config.StrategyConfig{
		ConfigID:     "cfg-1",
		Symbol:       "7203",
		StrategyName: config.StrategyTimeSeriesMomentum,
		HoldingMode:  order.HoldingMultiday,
	}
	c.Exit.TakeProfitJPY = 100
	c.Exit.StopLossJPY = 50
	c.Exit.MaxHoldMinutes = 240
	c.Risk.Quantity = 100
	c.Entry.Direction = config.DirectionBoth
	return c
}

func TestNoTrade_NeverEnters(t *testing.T) {
	sig := NoTrade{}.Evaluate(EvalInput{Now: time.Now(), Config: baseConfig()})
	if sig.IsEntry() {
		t.Fatal("no_trade must never enter")
	}
	if sig.Decision != DecisionNoTrade {
		t.Fatalf("decision = %q, want NO_TRADE", sig.Decision)
	}
}

// 床は tick で測る量(spread/slippage)だが、利確幅は円/株。TickSize で円へ直して比べる。
func TestCostFloor_ClearsFloor(t *testing.T) {
	cf := CostFloor{SpreadTicks: 2, SlippageTicks: 2, CarryTicks: 6, MinEdgeMultiple: 2, TickSize: 1}
	// intraday floor = 4 tick × 1円 = 4円; need tp >= 8円
	if cf.ClearsFloor(7, order.HoldingIntraday) {
		t.Fatal("tp 7円 should not clear intraday floor of 8円")
	}
	if !cf.ClearsFloor(8, order.HoldingIntraday) {
		t.Fatal("tp 8円 should clear intraday floor of 8円")
	}
	// multiday floor = 10 tick = 10円; need tp >= 20円
	if cf.ClearsFloor(19, order.HoldingMultiday) {
		t.Fatal("tp 19円 should not clear multiday floor of 20円")
	}
}

// 呼値が 5 円なら同じ tick 数の床でも必要な利確幅は 5 倍になる(床を tick のまま円と比べていた頃のバグを固定)。
func TestCostFloor_ScalesWithTickSize(t *testing.T) {
	cf := CostFloor{SpreadTicks: 2, SlippageTicks: 2, MinEdgeMultiple: 2, TickSize: 5}
	if cf.ClearsFloor(39, order.HoldingIntraday) {
		t.Fatal("tp 39円 should not clear a floor of 4tick × 5円 × 2 = 40円")
	}
	if !cf.ClearsFloor(40, order.HoldingIntraday) {
		t.Fatal("tp 40円 should clear")
	}
}

// 呼値不明(TickSize 0)は床を円に直せないので通す(fail-close にすると呼値が取れない瞬間に全戦略が沈黙する)。
func TestCostFloor_NoTickSizeSkipsCheck(t *testing.T) {
	cf := CostFloor{SpreadTicks: 100, SlippageTicks: 100, MinEdgeMultiple: 10}
	if !cf.ClearsFloor(1, order.HoldingIntraday) {
		t.Fatal("TickSize 未設定では床の比較をしない")
	}
}

func dailyTrendUp(n int) []market.Candle {
	cs := make([]market.Candle, n)
	for i := 0; i < n; i++ {
		p := 2000 + float64(i)*5 // steady uptrend
		cs[i] = market.Candle{Open: p, High: p + 3, Low: p - 3, Close: p}
	}
	// force the last bar to break above the recent Donchian high
	cs[n-1].Close = cs[n-1].High + 50
	return cs
}

func tsmInput(daily []market.Candle, spreadTicks float64) EvalInput {
	last := daily[len(daily)-1].Close
	return EvalInput{
		Now:          time.Now(),
		Config:       baseConfig(),
		CandlesDaily: daily,
		Summary: &market.MarketSummary{
			Symbol:      "7203",
			TickSize:    1,
			CurrentRate: market.CurrentRate{Last: last, SpreadTicks: spreadTicks},
		},
	}
}

func TestTimeSeriesMomentum_EntersOnTrendBreakout(t *testing.T) {
	in := tsmInput(dailyTrendUp(250), 2)
	sig := TimeSeriesMomentum{}.Evaluate(in)
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("expected BUY entry, got decision=%q side=%q reason=%q", sig.Decision, sig.Side, sig.Reason)
	}
}

func TestTimeSeriesMomentum_RejectsBelowCostFloor(t *testing.T) {
	in := tsmInput(dailyTrendUp(250), 60) // huge spread -> floor exceeds tp (100)
	sig := TimeSeriesMomentum{}.Evaluate(in)
	if sig.IsEntry() {
		t.Fatalf("entry should be rejected by cost floor, got %+v", sig)
	}
	if sig.Reason != "tp_below_cost_floor" {
		t.Fatalf("reason = %q, want tp_below_cost_floor", sig.Reason)
	}
}

func TestTimeSeriesMomentum_InsufficientHistory(t *testing.T) {
	in := tsmInput(dailyTrendUp(50), 2)
	sig := TimeSeriesMomentum{}.Evaluate(in)
	if sig.Decision != DecisionNoTrade {
		t.Fatalf("short history should be NO_TRADE, got %q", sig.Decision)
	}
}

func TestEngine_DispatchAndSignalID(t *testing.T) {
	eng := NewEngine(func() string { return "sig-123" }, TimeSeriesMomentum{})
	in := tsmInput(dailyTrendUp(250), 2)
	sig := eng.Evaluate(in)
	if !sig.IsEntry() {
		t.Fatalf("engine should dispatch to tsm and enter, got %q", sig.Decision)
	}
	if sig.SignalID != "sig-123" {
		t.Fatalf("SignalID = %q, want sig-123", sig.SignalID)
	}
}

func TestEngine_ExpiredConfig(t *testing.T) {
	cfg := baseConfig()
	cfg.TTLMinutes = 60
	cfg.ActivatedAt = time.Now().Add(-2 * time.Hour)
	eng := NewEngine(func() string { return "x" }, TimeSeriesMomentum{})
	in := tsmInput(dailyTrendUp(250), 2)
	in.Config = cfg
	sig := eng.Evaluate(in)
	if sig.Decision != DecisionNoTrade || sig.Reason != "config_expired" {
		t.Fatalf("expired config should NO_TRADE/config_expired, got %q/%q", sig.Decision, sig.Reason)
	}
}

func TestEngine_UnknownStrategyFailsSafe(t *testing.T) {
	cfg := baseConfig()
	cfg.StrategyName = config.StrategyName("does_not_exist")
	eng := NewEngine(func() string { return "x" })
	in := EvalInput{Now: time.Now(), Config: cfg}
	sig := eng.Evaluate(in)
	if sig.Decision != DecisionNoTrade {
		t.Fatalf("unknown strategy should NO_TRADE, got %q", sig.Decision)
	}
}
