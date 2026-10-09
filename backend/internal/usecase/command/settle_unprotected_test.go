package command

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/safety"
)

// 🚨 live で踏んだ事故。closeOne は守りの脚を **cancel してから** 決済を
// 出すので、決済が「受理されたのに板に載っていない」と建玉は**完全に裸**になる。
// 寄り直後の手動成行がまさにこれで、人間が証券アプリで TP を置き直すまで
// broker 側の守りがゼロだった。live の reconcile は保有中でも 1 時間おきなので、
// bot 側にこの窓を縮める手段が無かった。
//
// 「受理は約定ではない」は既に塞いである。**「受理は板に載っていることでもない」**が
// 残っていた穴で、結果は同じ(守りの消滅)なのに片方だけ黙っていた。
func TestCloseOne_UnfilledSettleNotRestingTripsEmergency(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 9, 7, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(now), 0, 0)
	pb.SetPrice("7203", 1000)
	brk := newAsyncSettleStub(pb, "close-1", notFilled)
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)

	openOne(t, ctx, brk, posRepo, es, clock.Fixed(now), order.HoldingMultiday, order.ExecMarginSystem)
	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	// 建玉時の守りは板に載っている(openOne がそれを検証する)。ここから先が事故の再現:
	// closeOne が守りを cancel し、決済は受理されたのに板に載らない。
	brk.hideOrders = true

	exec := closeExecutor{broker: brk, posRepo: posRepo, closer: repository.NewCloser(posRepo, trades), emergency: es}
	if ok, _ := exec.closeOne(ctx, open[0], 900, "manual", now); ok {
		t.Fatal("precondition: the settle must not be booked")
	}

	if !es.Active() {
		t.Fatal("守りを cancel した後に決済が板に無い = 建玉が裸。emergency を trip して人間を呼ばねばならない")
	}
}

// 逆に、決済注文が板に残っているなら trip しない。**未約定の決済を板に残すのは
// 意図的な設計**(ストップ安の引けは比例配分で、板に出ている売り注文にしか配分
// されない)。ここで trip すると、正常な待ちのたびに実弾が緊急停止する。
func TestCloseOne_UnfilledSettleStillRestingDoesNotTrip(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 9, 7, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(now), 0, 0)
	pb.SetPrice("7203", 1000)
	brk := newAsyncSettleStub(pb, "close-1", notFilled)
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)

	openOne(t, ctx, brk, posRepo, es, clock.Fixed(now), order.HoldingMultiday, order.ExecMarginSystem)
	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	// 受理された成行の決済注文が板に残っている状態。**逆指値脚は持たない**
	// (成行の返済注文なので) — ここを HasStopLeg で絞ると必ず取りこぼす。
	brk.restingOrders = []order.Order{{
		OrderID: "close-1", Symbol: "7203", Side: order.SideSell,
		Type: order.OrderTypeMarket, Quantity: 100, Status: "working",
	}}

	exec := closeExecutor{broker: brk, posRepo: posRepo, closer: repository.NewCloser(posRepo, trades), emergency: es}
	if ok, _ := exec.closeOne(ctx, open[0], 900, "manual", now); ok {
		t.Fatal("precondition: the settle must not be booked")
	}

	if es.Active() {
		t.Fatal("決済注文が板に残っているのは正常な待ち。trip してはいけない")
	}
}
