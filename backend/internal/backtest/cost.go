// Package backtest replays historical candles through the SAME production
// strategy / risk gate / exit logic under a stock cost floor (tick spread +
// slippage + carry + fee). Gross AND net are both emitted so the cost floor's
// bite stays visible. In-memory and deterministic; no DB or broker.
package backtest

import (
	"time"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
)

// Defaults are a modelled guess, NOT measured — recalibrate against live fills.
const (
	DefaultFeeRatePct    = 0.05 // one-side commission % (0.1% round trip)
	DefaultSlippageTicks = 0.0  // adverse ticks per leg on top of the spread
)

type SpreadModel interface {
	SpreadTicks(now time.Time) float64
}

type ConstSpread float64

func (c ConstSpread) SpreadTicks(time.Time) float64 { return float64(c) }

// CostModel turns price levels into adverse fills and round-trip costs. Tick
// size is never a constant here — always the JPX 呼値 tier for the price.
type CostModel struct {
	SlippageTicks float64
	FeeRatePct    float64 // one-side commission %
	Spread        SpreadModel
	// Carry は**本番と同じ** carry モデル(約定金額 × 年率 × 受渡日の両端入れ・D-12)。
	// 料率の入っていないゼロ値は carry 0(料率を捏造しない)。
	Carry position.CarryCalc
}

func (m CostModel) adverseTicks(now time.Time) float64 {
	half := 0.0
	if m.Spread != nil {
		half = m.Spread.SpreadTicks(now) / 2.0
	}
	return m.SlippageTicks + half
}

func (m CostModel) EntryFill(symbol string, side order.Side, barClose float64, now time.Time) float64 {
	adv := m.adverseTicks(now) * market.TickSizeOf(symbol, barClose)
	if side == order.SideBuy {
		return market.RoundToTickOf(symbol, barClose+adv)
	}
	return market.RoundToTickOf(symbol, barClose-adv)
}

// ExitFill fills adversely in the direction opposite the entry.
func (m CostModel) ExitFill(symbol string, side order.Side, touch float64, now time.Time) float64 {
	adv := m.adverseTicks(now) * market.TickSizeOf(symbol, touch)
	if side == order.SideBuy {
		return market.RoundToTickOf(symbol, touch-adv)
	}
	return market.RoundToTickOf(symbol, touch+adv)
}

func (m CostModel) FeeJPY(entryFill, exitFill float64, qty int) float64 {
	notional := (entryFill + exitFill) / 2.0 * float64(qty)
	return notional * (m.FeeRatePct / 100.0) * 2.0
}

// CarryJPY は建玉 p を closedAt に締めたときの carry(負 = コスト)。現物と執行区分の無い建玉は 0。
func (m CostModel) CarryJPY(p position.Position, closedAt time.Time) float64 {
	return m.Carry.JPY(p, closedAt)
}
