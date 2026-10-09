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

// 口座全体の「1 営業日の新規本数」の材料は**台帳から数える**。
// プロセスが死んで再起動しても同じ数になる(メモリに数を持たない)。
// 数えるのは今日建てた bot 建玉の全部(銘柄・戦略を問わない・決済済みも含む)。
// 前営業日の建玉と external は数えない。
func TestBuildStructural_CountsTodaysAccountEntriesFromTheLedger(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 15, 0, 0, clock.JST)
	h := newHarness(t, now)
	ctx := context.Background()
	ins := func(sym, strat string, src position.Source, opened time.Time) int64 {
		id, err := h.posRepo.Insert(ctx, port.PositionInsertInput{
			Symbol: sym, Side: order.SideBuy, Quantity: 100, EntryPrice: 2000,
			StrategyName: strat, HoldingMode: order.HoldingMultiday, Source: src, OpenedAt: opened,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	closedToday := ins("3186", "bnf_reversion", position.SourceBot, time.Date(2026, 10, 2, 9, 0, 5, 0, clock.JST))
	if ok, err := h.posRepo.ClaimForClose(ctx, closedToday, now); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	ins("6594", "bnf_day2_reversion_trail", position.SourceBot, time.Date(2026, 10, 2, 9, 30, 0, 0, clock.JST))
	ins("4385", "bnf_reversion", position.SourceBot, time.Date(2026, 10, 1, 9, 0, 0, 0, clock.JST)) // 前営業日
	ins("7203", "", position.SourceExternal, time.Date(2026, 10, 2, 9, 10, 0, 0, clock.JST))        // external

	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "5726", EntryPrice: 2400, Quantity: 100,
		StrategyName: "bnf_reversion", HoldingMode: order.HoldingMultiday,
	}
	build := func(capPerDay int) (count, cap int) {
		caps := SnapshotCaps{WindowMinutes: 60, AccountMaxEntriesPerDay: capPerDay}
		sb := NewSnapshotBuilder(h.posRepo, h.tradeRepo, h.broker, h.emergency, h.hours, clock.Fixed(now), caps)
		snap := sb.BuildStructural(ctx, "5726", &market.MarketSummary{}, sig, order.ExecMarginSystem)
		return snap.AccountEntriesToday, snap.AccountMaxEntriesPerDay
	}
	if n, c := build(2); n != 2 || c != 2 {
		t.Fatalf("今日の口座全体の建て = %d(上限 %d), want 2(上限 2)— 前営業日と external は数えない", n, c)
	}
	// 上限 0(research)は数えない = 毎ティックの往復を足さない。
	if n, c := build(0); n != 0 || c != 0 {
		t.Fatalf("上限 0 なのに数えた: %d(上限 %d)", n, c)
	}
}
