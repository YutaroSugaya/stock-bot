package command

import (
	"context"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
)

// ForceFlatten MARKET-closes intraday positions before the bell — the invariant
// that avoids the carry-over 固定ペナルティ. A close rejection trips emergency so a
// stuck position is surfaced immediately.
type ForceFlatten struct {
	exec      closeExecutor
	emergency EmergencyController
	hours     session.TradingHours
	clock     clock.Clock
}

func NewForceFlatten(b port.Broker, pr port.PositionRepository, closer port.PositionCloser, em EmergencyController, hours session.TradingHours, c clock.Clock) *ForceFlatten {
	if c == nil {
		c = clock.System()
	}
	return &ForceFlatten{
		exec:      closeExecutor{broker: b, posRepo: pr, closer: closer, emergency: em},
		emergency: em,
		hours:     hours,
		clock:     c,
	}
}

// WithCarry: 未設定なら CarryJPY は 0 のまま — 料率を捏造しない。
func (f *ForceFlatten) WithCarry(c position.CarryCalc) *ForceFlatten {
	f.exec.carry = c
	return f
}

func (f *ForceFlatten) FlattenIfNearClose(ctx context.Context, symbol string, summary *market.MarketSummary) (int, error) {
	now := f.clock()
	if !f.hours.IsNearClose(now) {
		return 0, nil
	}
	return f.Flatten(ctx, symbol, summary)
}

// Flatten force-closes regardless of the clock; FlattenIfNearClose gates on it.
func (f *ForceFlatten) Flatten(ctx context.Context, symbol string, summary *market.MarketSummary) (int, error) {
	now := f.clock()
	price := observedPrice(summary)
	positions, err := f.exec.posRepo.ListOpenOrClosing(ctx, symbol)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, p := range positions {
		if p.Status != position.StatusOpen || p.HoldingMode != order.HoldingIntraday {
			continue
		}
		// External positions are display-only EXCEPT 一日信用: the carry-over penalty
		// is unconditional, so an adopted oneday position (e.g. a bot orphan
		// re-adopted after a crash) must still be flattened.
		if p.Source == position.SourceExternal && p.ExecKind != order.ExecMarginOneday {
			continue
		}
		ok, err := f.exec.closeOne(ctx, p, price, "forced_flat", now)
		if err != nil || !ok {
			if f.emergency != nil {
				_ = f.emergency.Trip("forced_liquidation_failed", now)
			}
			return closed, err
		}
		closed++
	}
	return closed, nil
}
