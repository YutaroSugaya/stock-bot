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

// 口座全体の「1 営業日の新規本数」の材料。銘柄・戦略を問わず、決済済みも数え、
// 前営業日と external は数えない。
func TestPg_CountOpenedSince(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()
	if err := NewConfigRepo(pool).EnsureExists(ctx, port.StrategyConfigRecord{ConfigID: "cfg-day", Symbol: "*", Mode: "live_config", StrategyName: "bnf_reversion", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	repo := NewPositionRepo(pool)
	day := time.Date(2026, 10, 2, 0, 0, 0, 0, clock.JST)
	ins := func(sym, strat string, src position.Source, opened time.Time) int64 {
		id, err := repo.Insert(ctx, port.PositionInsertInput{
			Symbol: sym, Side: order.SideBuy, Quantity: 100, EntryPrice: 2000, StrategyName: strat,
			StrategyConfigID: "cfg-day", HoldingMode: order.HoldingMultiday, ExecKind: order.ExecMarginSystem, Source: src, OpenedAt: opened,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	closed := ins("3186", "bnf_reversion", position.SourceBot, day.Add(9*time.Hour))
	if ok, err := repo.ClaimForClose(ctx, closed, day.Add(10*time.Hour)); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	ins("6594", "bnf_day2_reversion_trail", position.SourceBot, day.Add(9*time.Hour+30*time.Minute))
	ins("4385", "bnf_reversion", position.SourceBot, day.Add(-24*time.Hour))
	ins("7203", "", position.SourceExternal, day.Add(9*time.Hour+10*time.Minute))
	n, err := repo.CountOpenedSince(ctx, day)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v, want 2", n, err)
	}
}
