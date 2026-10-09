package strategy

import (
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/ta"
)

// backtest 較正待ちのプレースホルダ定数(証明済みエッジではない)。
const (
	tsmTrendLookback    = 200 // daily SMA lookback for the long-term trend
	tsmBreakoutLookback = 20  // Donchian breakout channel
	tsmSlippageTicks    = 2.0 // assumed adverse-fill ticks (calibrate vs live)
	tsmMinEdgeMultiple  = 2.0 // tp must beat round-trip floor by this much (track B/A safety)
)

// 長期トレンド × 新規ブレイク × コスト床のテンプレ。エッジ検定の候補であって確定戦略ではない。
type TimeSeriesMomentum struct{}

func (TimeSeriesMomentum) Name() config.StrategyName { return config.StrategyTimeSeriesMomentum }

func (s TimeSeriesMomentum) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, tsmTrendLookback+1); !ok {
		return sig
	}
	daily := in.CandlesDaily

	cl := make([]float64, len(daily))
	for i, c := range daily {
		cl[i] = c.Close
	}
	sma := ta.SMA(cl, tsmTrendLookback)
	slope := ta.Slope(cl, tsmTrendLookback)
	last := daily[len(daily)-1].Close

	// live バーを除いた確定足でブレイクを確認する(no look-ahead)。
	prior := daily[:len(daily)-1]
	hi, lo, ok := ta.Donchian(prior, tsmBreakoutLookback)
	if !ok {
		return noTradeSignal(in, name, "insufficient_breakout_history")
	}

	var side order.Side
	switch {
	case last > sma && slope > 0 && last >= hi:
		side = order.SideBuy
	case last < sma && slope < 0 && last <= lo:
		side = order.SideSell
	default:
		return noTradeSignal(in, name, "no_trend_breakout")
	}

	switch in.Config.Entry.Direction {
	case config.DirectionBuyOnly:
		if side != order.SideBuy {
			return noTradeSignal(in, name, "direction_buy_only")
		}
	case config.DirectionSellOnly:
		if side != order.SideSell {
			return noTradeSignal(in, name, "direction_sell_only")
		}
	}

	// 出口は ATR スケール。ATR が取れなければ円幅に落ちず no_trade。
	atr := ta.ATR(daily, atrExitPeriod)
	if atr <= 0 {
		return noTradeSignal(in, name, "no_atr")
	}
	sig := atrExitSignal(in, side, in.Summary.CurrentRate.Last, name, "tsm_trend_breakout", atr)
	if sig.HoldingMode == "" {
		sig.HoldingMode = order.HoldingMultiday // daily trend is a track-B candidate
	}

	floor := CostFloor{
		SpreadTicks:     in.Summary.CurrentRate.SpreadTicks,
		SlippageTicks:   tsmSlippageTicks,
		CarryTicks:      0, // multiday carry calibrated by backtest; 0 = not yet modelled
		MinEdgeMultiple: tsmMinEdgeMultiple,
		TickSize:        summaryTickSize(in),
	}
	return applyCostFloor(in, sig, floor)
}
