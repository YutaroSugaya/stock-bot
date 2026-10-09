package repository

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// 🛑 同じ権利落ち日に二度割らない(判定はその日のうち毎ティック成立する)。
func TestInMemoryPositionRepo_ApplySplitOncePerDay(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryPositionRepo()
	id, _ := r.Insert(ctx, port.PositionInsertInput{Symbol: "6368", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 11570, StopLossPrice: 10570, StopLossJPY: 1000, OpenedAt: time.Now()})
	p, _ := r.GetByID(ctx, id)
	day := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	adj, err := position.SplitAdjusted(*p, 5, day)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := r.ApplySplit(ctx, adj); err != nil || !ok {
		t.Fatalf("1 回目 = (%v, %v), want (true, nil)", ok, err)
	}
	got, _ := r.GetByID(ctx, id)
	if got.Quantity != 500 || got.EntryPrice != 2314 || got.StopLossPrice != 2114 || got.SplitFactor != 5 {
		t.Fatalf("書けていない: %+v", got)
	}
	// 同じ日の 2 回目(再起動後・次のティック)は拒否。
	again, _ := position.SplitAdjusted(*got, 5, day)
	if ok, _ := r.ApplySplit(ctx, again); ok {
		t.Fatal("同じ権利落ち日に二度割った(1:5 が 1:25 になる)")
	}
	if g, _ := r.GetByID(ctx, id); g.Quantity != 500 {
		t.Fatalf("拒否したのに書き換わった: qty %d", g.Quantity)
	}
}

func TestInMemoryPositionRepo_ApplySplitOnlyOpen(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryPositionRepo()
	id, _ := r.Insert(ctx, port.PositionInsertInput{Symbol: "6368", Side: order.SideBuy, Quantity: 100, EntryPrice: 1000, OpenedAt: time.Now()})
	_, _ = r.ClaimForClose(ctx, id, time.Now())
	p, _ := r.GetByID(ctx, id)
	adj, _ := position.SplitAdjusted(*p, 2, time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC))
	if ok, _ := r.ApplySplit(ctx, adj); ok {
		t.Fatal("CLOSING の建玉を分割調整した(決済中の数量が変わる)")
	}
}
