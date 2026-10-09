package command

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// 日中の「同日 1 回転」の材料: **その営業日に** (銘柄, 戦略) で建てた bot 建玉の数。
// 決済済みも数える(1 本目を閉じた後に同じ銘柄を買い直す経路を止めるため)。
func TestBuildStructural_CountsTodaysIntradayEntriesPerStrategy(t *testing.T) {
	now := time.Date(2026, 9, 14, 14, 25, 33, 0, clock.JST)
	h := newHarness(t, now)
	ctx := context.Background()
	ins := func(strat string, opened time.Time) int64 {
		id, err := h.posRepo.Insert(ctx, port.PositionInsertInput{
			Symbol: "3186", Side: order.SideBuy, Quantity: 100, EntryPrice: 2868,
			StrategyName: strat, HoldingMode: order.HoldingIntraday, Source: position.SourceBot, OpenedAt: opened,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	today := ins("bnf_intraday_reversion", time.Date(2026, 9, 14, 9, 25, 0, 0, clock.JST))
	if ok, err := h.posRepo.ClaimForClose(ctx, today, now); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	ins("bnf_intraday_reversion", time.Date(2026, 9, 11, 9, 30, 0, 0, clock.JST)) // 前営業日
	ins("bnf_intraday_reversion_trail", time.Date(2026, 9, 14, 9, 25, 0, 0, clock.JST))

	sb := NewSnapshotBuilder(h.posRepo, h.tradeRepo, h.broker, h.emergency, h.hours, clock.Fixed(now), SnapshotCaps{WindowMinutes: 60})
	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "3186", EntryPrice: 2937, Quantity: 100,
		StrategyName: "bnf_intraday_reversion", HoldingMode: order.HoldingIntraday,
	}
	snap := sb.BuildStructural(ctx, "3186", &market.MarketSummary{}, sig, order.ExecCash)
	if snap.EntriesTodaySameStrategy != 1 {
		t.Fatalf("今日の同一戦略の建て = %d, want 1(前営業日と兄弟 _trail は数えない)", snap.EntriesTodaySameStrategy)
	}
}
