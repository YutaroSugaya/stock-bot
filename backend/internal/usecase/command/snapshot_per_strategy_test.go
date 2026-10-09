package command

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// ナンピン禁止のキーが (銘柄, 側, 戦略) になったので、snapshot は
// 「同一戦略で何本開いているか」を数えなければならない。数え方を間違えると、
// ペアが 1 本も建たない(全部ブロック)か、同一戦略が積み増せる(不変条件違反)。

type perStrategyFixture struct {
	sb      *SnapshotBuilder
	posRepo *repository.InMemoryPositionRepo
}

func newPerStrategyFixture(t *testing.T) *perStrategyFixture {
	t.Helper()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, clock.JST)
	c := clock.Fixed(now)
	posRepo := repository.NewInMemoryPositionRepo()
	sb := NewSnapshotBuilder(posRepo, repository.NewInMemoryTradeRepo(), broker.NewPaper(c, 1, 0),
		nil, tokyoHours(), c, SnapshotCaps{})
	return &perStrategyFixture{sb: sb, posRepo: posRepo}
}

func (f *perStrategyFixture) open(t *testing.T, name config.StrategyName, side order.Side, src position.Source) {
	t.Helper()
	_, err := f.posRepo.Insert(context.Background(), port.PositionInsertInput{
		BrokerPositionID: string(name) + string(side) + string(src), Symbol: "7203", Side: side,
		Quantity: 100, EntryPrice: 2000, StrategyConfigID: "cfg", StrategyName: string(name),
		Source: src, OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func (f *perStrategyFixture) snap(name config.StrategyName, side order.Side) (all, same int) {
	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Symbol: "7203", Side: side, Quantity: 100,
		EntryPrice: 2000, StrategyName: name,
	}
	s := f.sb.BuildStructural(context.Background(), "7203", nil, sig, order.ExecCash)
	if side == order.SideSell {
		return s.OpenSellInclExternal, s.OpenSellSameStrategyInclExternal
	}
	return s.OpenBuyInclExternal, s.OpenBuySameStrategyInclExternal
}

func TestSnapshotCountsSameStrategyOnly(t *testing.T) {
	f := newPerStrategyFixture(t)
	f.open(t, config.StrategyBNFReversion, order.SideBuy, position.SourceBot)

	all, same := f.snap(config.StrategyBNFReversionTrail, order.SideBuy)
	if all != 1 {
		t.Fatalf("従来の同一側カウント = %d, want 1", all)
	}
	if same != 0 {
		t.Fatalf("別戦略なのに同一戦略として数えている: %d", same)
	}

	if _, same = f.snap(config.StrategyBNFReversion, order.SideBuy); same != 1 {
		t.Fatalf("同一戦略のカウント = %d, want 1", same)
	}
}

// 🛑 external は**戦略が分からない**ので、どの戦略に対しても数える(fail-close)。
// 「戦略が違う」と読むと、人間が持っている建玉と二重に持つ。
func TestSnapshotCountsExternalPositionsForEveryStrategy(t *testing.T) {
	f := newPerStrategyFixture(t)
	f.open(t, "", order.SideBuy, position.SourceExternal)

	for _, name := range []config.StrategyName{
		config.StrategyBNFReversion, config.StrategyAbsMomentumV2, config.StrategyDonchianBreakoutV2,
	} {
		if _, same := f.snap(name, order.SideBuy); same != 1 {
			t.Errorf("%s: external 建玉を数えていない(same=%d)", name, same)
		}
	}
}

// 戦略不明("" = migration 0015 の backfill が届かなかった旧建玉)も
// 同じく全戦略に対して数える。
func TestSnapshotCountsUnknownStrategyPositionsForEveryStrategy(t *testing.T) {
	f := newPerStrategyFixture(t)
	f.open(t, "", order.SideBuy, position.SourceBot)

	if _, same := f.snap(config.StrategyAbsMomentumV2, order.SideBuy); same != 1 {
		t.Fatalf("戦略不明の旧建玉を数えていない(same=%d) — 二重に持つ", same)
	}
}

// 側は混ざらない(両建てが起きうる)。
func TestSnapshotKeepsSidesSeparate(t *testing.T) {
	f := newPerStrategyFixture(t)
	f.open(t, config.StrategyAbsMomentumV2, order.SideBuy, position.SourceBot)

	if _, same := f.snap(config.StrategyAbsMomentumV2, order.SideSell); same != 0 {
		t.Fatalf("買い建玉が売りのナンピン判定に混ざっている(same=%d)", same)
	}
	if _, same := f.snap(config.StrategyAbsMomentumV2, order.SideBuy); same != 1 {
		t.Fatalf("買いのカウントが落ちている(same=%d)", same)
	}
}
