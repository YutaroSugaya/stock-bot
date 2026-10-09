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

// 日中の「同日 1 回転」の材料。決済済みも数え、前営業日・別戦略・external は数えない。
func TestPg_CountOpenedSinceBySymbolStrategy(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()
	if err := NewConfigRepo(pool).EnsureExists(ctx, port.StrategyConfigRecord{ConfigID: "cfg-once", Symbol: "3186", Mode: "paper_config", StrategyName: "bnf_intraday_reversion", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	repo := NewPositionRepo(pool)
	day := time.Date(2026, 9, 14, 0, 0, 0, 0, clock.JST)
	ins := func(strat string, src position.Source, opened time.Time) int64 {
		id, err := repo.Insert(ctx, port.PositionInsertInput{
			Symbol: "3186", Side: order.SideBuy, Quantity: 100, EntryPrice: 2868, StrategyName: strat,
			StrategyConfigID: "cfg-once", HoldingMode: order.HoldingIntraday, ExecKind: order.ExecCash, Source: src, OpenedAt: opened,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	closed := ins("bnf_intraday_reversion", position.SourceBot, day.Add(9*time.Hour+25*time.Minute))
	if ok, err := repo.ClaimForClose(ctx, closed, day.Add(14*time.Hour)); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	ins("bnf_intraday_reversion", position.SourceBot, day.Add(-3*24*time.Hour))
	ins("bnf_intraday_reversion_trail", position.SourceBot, day.Add(10*time.Hour))
	ins("bnf_intraday_reversion", position.SourceExternal, day.Add(10*time.Hour))
	n, err := repo.CountOpenedSinceBySymbolStrategy(ctx, "3186", "bnf_intraday_reversion", day)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v, want 1", n, err)
	}
}
