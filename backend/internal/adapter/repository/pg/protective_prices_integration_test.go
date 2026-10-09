//go:build integration

package pg

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 画面の「TP/SL 変更」が台帳も揃える(4901)。OPEN 以外は触らない。
func TestPg_UpdateProtectivePrices(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()
	if err := NewConfigRepo(pool).EnsureExists(ctx, port.StrategyConfigRecord{ConfigID: "cfg-pp", Symbol: "4901", Mode: "live_config", StrategyName: "bnf_reversion", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	repo := NewPositionRepo(pool)
	id, err := repo.Insert(ctx, port.PositionInsertInput{
		Symbol: "4901", Side: order.SideBuy, Quantity: 100, EntryPrice: 3281, TakeProfitPrice: 3721, StopLossPrice: 2986.5,
		StrategyConfigID: "cfg-pp", HoldingMode: order.HoldingMultiday, ExecKind: order.ExecMarginSystem, OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.UpdateProtectivePrices(ctx, id, 3505, 3115, 224, 166); err != nil || !ok {
		t.Fatalf("update: ok=%v err=%v", ok, err)
	}
	p, _ := repo.GetByID(ctx, id)
	if p.TakeProfitPrice != 3505 || p.StopLossPrice != 3115 || p.TakeProfitJPY != 224 || p.StopLossJPY != 166 {
		t.Fatalf("台帳 = %+v", p)
	}
	if ok, _ := repo.ClaimForClose(ctx, id, time.Now()); !ok {
		t.Fatal("claim")
	}
	if ok, err := repo.UpdateProtectivePrices(ctx, id, 1, 1, 1, 1); err != nil || ok {
		t.Fatalf("CLOSING の建玉を更新した: ok=%v err=%v", ok, err)
	}
}
