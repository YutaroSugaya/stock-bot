package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// MOCK rationale (TESTING.md §1 system boundary): 台帳エラーの枝は実装からは作れない。
type erroringPositions struct {
	port.PositionRepository
}

func (erroringPositions) ListOpenOrClosing(context.Context, string) ([]position.Position, error) {
	return nil, errors.New("db down")
}

// arm 判定のキーは **(銘柄, 戦略)**。銘柄だけで見ると、片方のアームが建った瞬間に
// 同じ銘柄のもう片方が永久に arm されない(ペアが 0 本になる原因)。
func TestHeldByStrategy(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, clock.JST)
	repo := repository.NewInMemoryPositionRepo()
	if _, err := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "b1", Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		OpenedAt: now, StrategyName: "abs_momentum_v2",
	}); err != nil {
		t.Fatal(err)
	}
	held := heldByStrategy(repo)

	if !held(ctx, "7203", config.StrategyName("abs_momentum_v2")) {
		t.Error("同じ戦略の建玉を建玉中と見ていない")
	}
	// 🛑 兄弟アームは**別扱い**(ここが同一だとペアが構造的に成立しない)。
	if held(ctx, "7203", config.StrategyName("abs_momentum_v2_trail")) {
		t.Error("兄弟アームまで建玉中と見た — ペアが 1 本も建たない")
	}
	if held(ctx, "6501", config.StrategyName("abs_momentum_v2")) {
		t.Error("建玉の無い銘柄を建玉中と見た")
	}
}

// 🛑 **戦略不明の建玉は全戦略をブロックする**(fail-close)。external と、backfill が
// 届かなかった旧建玉が該当。「戦略が違う」と読むと人間の建玉と二重に持つ。
func TestHeldByStrategy_UnknownStrategyBlocksEveryArm(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	if _, err := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "ext", Symbol: "4704", Side: order.SideBuy, Quantity: 100,
		OpenedAt: time.Now(), // StrategyName 空 = 戦略不明
	}); err != nil {
		t.Fatal(err)
	}
	held := heldByStrategy(repo)
	for _, name := range []string{"abs_momentum_v2", "bnf_reversion", "high_52w_momentum_trail"} {
		if !held(ctx, "4704", config.StrategyName(name)) {
			t.Errorf("戦略不明の建玉がある銘柄で %s を arm 可能と見た — 人間の建玉と二重に持つ", name)
		}
	}
}

// 🚨 **台帳が読めないときは「建玉中」に倒す**。以前は main.go の
// インライン閉包で false に落ちており、DB 不調のときに建玉中の銘柄を「空いている」と
// 読んで arm していた。分からないなら建てない側。
func TestHeldByStrategy_FailsClosedWhenTheLedgerIsUnreadable(t *testing.T) {
	held := heldByStrategy(erroringPositions{})
	if !held(context.Background(), "7203", config.StrategyName("abs_momentum_v2")) {
		t.Fatal("台帳エラーで false(= 空いている)を返した — 建玉中の銘柄を arm する")
	}
}
