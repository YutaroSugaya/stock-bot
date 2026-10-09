//go:build integration

package pg

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// 株式分割の言い直し(migration 0022)。日付が JST のまま往復し、同じ日に二度割らないこと。
func TestPg_ApplySplitOncePerDay(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()
	if err := NewConfigRepo(pool).EnsureExists(ctx, port.StrategyConfigRecord{ConfigID: "cfg-split", Symbol: "6368", Mode: "paper_config", StrategyName: "bnf_day2_reversion", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	repo := NewPositionRepo(pool)
	id, err := repo.Insert(ctx, port.PositionInsertInput{
		Symbol: "6368", Side: order.SideBuy, Quantity: 100, EntryPrice: 11570, TakeProfitPrice: 13470, StopLossPrice: 10570,
		TakeProfitJPY: 1900, StopLossJPY: 1000, StrategyConfigID: "cfg-split",
		HoldingMode: order.HoldingMultiday, ExecKind: order.ExecMarginSystem, OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := repo.GetByID(ctx, id)
	if p.SplitFactor != 1 || !p.SplitAdjustedOn.IsZero() {
		t.Fatalf("既定値 = factor %v on %v, want 1 / zero", p.SplitFactor, p.SplitAdjustedOn)
	}
	// 🛑 JST の 0 時(UTC では前日 15 時)を渡しても、DB の日付は JST の日付のまま。
	day := time.Date(2026, 9, 29, 0, 0, 0, 0, clock.JST)
	adj, err := position.SplitAdjusted(*p, 5, day)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.ApplySplit(ctx, adj); err != nil || !ok {
		t.Fatalf("1 回目: ok=%v err=%v", ok, err)
	}
	got, _ := repo.GetByID(ctx, id)
	if got.Quantity != 500 || got.EntryPrice != 2314 || got.StopLossPrice != 2114 || got.TakeProfitJPY != 380 || got.SplitFactor != 5 {
		t.Fatalf("台帳 = %+v", got)
	}
	if d := got.SplitAdjustedOn.Format(time.DateOnly); d != "2026-09-29" {
		t.Fatalf("split_adjusted_on = %s, want 2026-09-29(JST の日付)", d)
	}
	again, _ := position.SplitAdjusted(*got, 5, day)
	if ok, err := repo.ApplySplit(ctx, again); err != nil || ok {
		t.Fatalf("同じ日に二度割った: ok=%v err=%v", ok, err)
	}
	if g, _ := repo.GetByID(ctx, id); g.Quantity != 500 {
		t.Fatalf("qty %d", g.Quantity)
	}
}
