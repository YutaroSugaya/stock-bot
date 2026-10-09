package backtest

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

// dailyProbe records what daily window the engine hands an intraday strategy and
// enters whenever ANY daily context is visible, so one replay checks both
// delivery and the no-look-ahead cut.
type dailyProbe struct {
	sawDaily  *int
	violation *string
}

func (dailyProbe) Name() config.StrategyName { return config.StrategyName("daily_probe") }
func (p dailyProbe) Evaluate(in strategy.EvalInput) strategy.Signal {
	if len(in.CandlesDaily) == 0 {
		return strategy.Signal{Decision: strategy.DecisionNoTrade, Reason: "no_daily"}
	}
	*p.sawDaily++
	barDay := in.Now.In(clock.JST).Format("2006-01-02")
	for _, d := range in.CandlesDaily {
		if day := d.OpenTime.In(clock.JST).Format("2006-01-02"); day >= barDay {
			*p.violation = "daily bar " + day + " visible during session " + barDay
		}
	}
	return strategy.Signal{
		Decision: strategy.DecisionEnter, Symbol: in.Config.Symbol, Side: order.SideBuy,
		EntryPrice: in.Summary.CurrentRate.Last, TakeProfitJPY: 5, StopLossJPY: 5,
		Quantity: 100, HoldingMode: order.HoldingIntraday, ConfigID: in.Config.ConfigID, CreatedAt: in.Now,
	}
}

func TestReplay_DailyContextFeedsIntradayStrategy(t *testing.T) {
	// Spans the replay days AND includes them, which is what loading a daily CSV
	// alongside a 5m CSV produces — the case the look-ahead cut has to handle.
	var daily []market.Candle
	for i := 0; i < 30; i++ {
		day := time.Date(2026, 6, 1, 9, 0, 0, 0, clock.JST).AddDate(0, 0, i)
		daily = append(daily, market.Candle{
			Symbol: "X", OpenTime: day, Interval: 24 * time.Hour,
			Open: 1000, High: 1005, Low: 995, Close: 1000, Volume: 1000,
		})
	}
	var m5 []market.Candle
	for _, d := range []time.Time{
		time.Date(2026, 6, 25, 9, 0, 0, 0, clock.JST),
		time.Date(2026, 6, 26, 9, 0, 0, 0, clock.JST),
	} {
		for i := 0; i < 12; i++ {
			m5 = append(m5, market.Candle{
				Symbol: "X", OpenTime: d.Add(time.Duration(i) * 5 * time.Minute), Interval: 5 * time.Minute,
				Open: 1000, High: 1001, Low: 999, Close: 1000, Volume: 100,
			})
		}
	}

	saw := 0
	violation := ""
	cfg := &config.StrategyConfig{ConfigID: "bt", Symbol: "X", StrategyName: config.StrategyName("daily_probe"), HoldingMode: order.HoldingIntraday}
	cfg.Entry.Direction = config.DirectionBoth
	cfg.Risk.Quantity = 100
	cfg.Risk.MaxOpenPositions = 1

	e := NewEngine(cfg, dailyProbe{sawDaily: &saw, violation: &violation}, CostModel{}, PessimisticSLFirst)
	e.DailyContext = daily
	if _, err := e.Replay(context.Background(), m5); err != nil {
		t.Fatal(err)
	}
	if saw == 0 {
		t.Fatal("intraday replay never received the daily context")
	}
	if violation != "" {
		t.Fatalf("look-ahead: %s", violation)
	}
}

func TestReplay_NoDailyContextKeepsCandlesDailyEmpty(t *testing.T) {
	saw := 0
	violation := ""
	cfg := &config.StrategyConfig{ConfigID: "bt", Symbol: "X", StrategyName: config.StrategyName("daily_probe"), HoldingMode: order.HoldingIntraday}
	cfg.Entry.Direction = config.DirectionBoth
	cfg.Risk.Quantity = 100

	base := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	var m5 []market.Candle
	for i := 0; i < 10; i++ {
		m5 = append(m5, market.Candle{
			Symbol: "X", OpenTime: base.Add(time.Duration(i) * 5 * time.Minute), Interval: 5 * time.Minute,
			Open: 1000, High: 1001, Low: 999, Close: 1000, Volume: 100,
		})
	}
	e := NewEngine(cfg, dailyProbe{sawDaily: &saw, violation: &violation}, CostModel{}, PessimisticSLFirst)
	if _, err := e.Replay(context.Background(), m5); err != nil {
		t.Fatal(err)
	}
	if saw != 0 {
		t.Fatalf("no DailyContext was configured, yet the strategy saw daily bars %d times", saw)
	}
}
