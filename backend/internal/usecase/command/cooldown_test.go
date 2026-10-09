package command

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// 実測: 6976 が 09:06:05 建玉 → 47秒後に損切り → **同じ秒に再エントリー**
// (3436 も同様)。cooldown は hard_limits にも risk gate にもあるのに**値を設定する
// 場所が無く常に false** = 機構ごと死んでいた。
func TestSnapshot_CooldownAfterLoss(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, clock.JST)
	tradeRepo := repository.NewInMemoryTradeRepo()
	if _, err := tradeRepo.Insert(ctx, port.TradeRecord{
		Symbol: "6976", Side: order.SideBuy, Quantity: 100, EntryPrice: 10715, ClosePrice: 10410,
		ProfitLossJPY: -30500, CloseReason: "stop_loss", ClosedAt: now.Add(-5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	b := newCooldownBuilder(t, tradeRepo, now, 1800, 0)

	snap := b.BuildStructural(ctx, "6976", nil, strategy.Signal{Decision: strategy.DecisionEnter, Symbol: "6976", Side: order.SideBuy, Quantity: 100}, order.ExecCash)
	if !snap.InCooldown {
		t.Fatalf("損切り5分後にクールダウンが効いていない: %+v", snap)
	}
	if snap.CooldownKind != "after_loss" {
		t.Errorf("CooldownKind = %q, want after_loss", snap.CooldownKind)
	}
	want := now.Add(-5 * time.Minute).Add(1800 * time.Second)
	if !snap.CooldownUntil.Equal(want) {
		t.Errorf("CooldownUntil = %v, want %v", snap.CooldownUntil, want)
	}
}

func TestSnapshot_CooldownExpires(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, clock.JST)
	tradeRepo := repository.NewInMemoryTradeRepo()
	if _, err := tradeRepo.Insert(ctx, port.TradeRecord{
		Symbol: "6976", Side: order.SideBuy, Quantity: 100, EntryPrice: 100, ClosePrice: 90,
		ProfitLossJPY: -1000, CloseReason: "stop_loss", ClosedAt: now.Add(-31 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	b := newCooldownBuilder(t, tradeRepo, now, 1800, 0)
	if snap := b.BuildStructural(ctx, "6976", nil, strategy.Signal{}, order.ExecCash); snap.InCooldown {
		t.Fatalf("31分後もクールダウンが解けていない: %+v", snap)
	}
}

// 勝ったセットアップは「壊れた」わけではないので、損切り後と同じ扱いにしない。
func TestSnapshot_CooldownAfterTakeProfitIsSeparate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, clock.JST)
	tradeRepo := repository.NewInMemoryTradeRepo()
	if _, err := tradeRepo.Insert(ctx, port.TradeRecord{
		Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 100, ClosePrice: 110,
		ProfitLossJPY: 1000, CloseReason: "take_profit", ClosedAt: now.Add(-1 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if snap := newCooldownBuilder(t, tradeRepo, now, 1800, 0).BuildStructural(ctx, "7203", nil, strategy.Signal{}, order.ExecCash); snap.InCooldown {
		t.Fatalf("勝ちトレード後に損切りクールダウンが誤適用された: %+v", snap)
	}
	if snap := newCooldownBuilder(t, tradeRepo, now, 0, 600).BuildStructural(ctx, "7203", nil, strategy.Signal{}, order.ExecCash); !snap.InCooldown || snap.CooldownKind != "after_take_profit" {
		t.Fatalf("after_take_profit が効いていない: %+v", snap)
	}
}

func TestSnapshot_NoTradesNoCooldown(t *testing.T) {
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, clock.JST)
	b := newCooldownBuilder(t, repository.NewInMemoryTradeRepo(), now, 1800, 600)
	if snap := b.BuildStructural(context.Background(), "9999", nil, strategy.Signal{}, order.ExecCash); snap.InCooldown {
		t.Fatalf("決済履歴なしでクールダウン: %+v", snap)
	}
}

func newCooldownBuilder(t *testing.T, tr port.TradeRepository, now time.Time, afterLoss, afterTP int) *SnapshotBuilder {
	t.Helper()
	hours := session.TradingHours{
		TZ: clock.JST, Sessions: []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:30"}},
		EntryCutoff: "14:55", ForceFlatAt: "14:50",
	}
	pb := broker.NewPaper(clock.Fixed(now), 0, 0)
	b := NewSnapshotBuilder(repository.NewInMemoryPositionRepo(), tr, pb, nil, hours, clock.Fixed(now), SnapshotCaps{})
	return b.WithCooldown(CooldownPolicy{AfterLossSeconds: afterLoss, AfterTakeProfitSeconds: afterTP})
}
