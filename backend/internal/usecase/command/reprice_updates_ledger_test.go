package command

import (
	"context"
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 🚨 画面の「TP/SL 変更」は**板だけ**を変えて台帳を更新していなかった(例:
// 人間が締めた SL 3115 / TP 3505 が板にあり、台帳は建玉時の SL 2986.5 / TP 3721 のまま)。
// 台帳が古いと、板の守りが消えたとき RearmUnguarded が**緩い凍結値で置き直し**、多日の TP は
// OnTick が古い値で判定する。人間が値段を変えたら台帳も同じ値にする。
func repriceLedgerFixture(t *testing.T) (*repository.InMemoryPositionRepo, int64, *fakeReplaceBroker) {
	t.Helper()
	repo := repository.NewInMemoryPositionRepo()
	id, err := repo.Insert(context.Background(), port.PositionInsertInput{
		BrokerPositionID: "b-4901", Symbol: "4901", Side: order.SideBuy, Quantity: 100, EntryPrice: 3281,
		TakeProfitPrice: 3721, StopLossPrice: 2986.5, TakeProfitJPY: 440, StopLossJPY: 294.5,
		OpenedAt: time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST), HoldingMode: order.HoldingMultiday,
		MaxHoldMinutes: 14400, ExecKind: order.ExecMarginSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{"4901": {guardAtCeiling()}}},
		requireExec:      order.ExecMarginSystem,
	}
	return repo, id, brk
}

func TestRepriceProtectiveOrder_UpdatesTheLedgerToTheNewPrices(t *testing.T) {
	repo, id, brk := repriceLedgerFixture(t)
	if _, err := newReprice(repo, brk, &fakeTripper{}).Execute(context.Background(),
		RepriceProtectiveInput{Symbol: "4901", TakeProfit: 3505, StopLoss: 3115}); err != nil {
		t.Fatalf("err = %v", err)
	}
	p, _ := repo.GetByID(context.Background(), id)
	// 板は stop-only(多日)でも、台帳の TP は人間が指定した値 — OnTick がそれで利確を判定する。
	if p.TakeProfitPrice != 3505 || p.StopLossPrice != 3115 || p.TakeProfitJPY != 224 || p.StopLossJPY != 166 {
		t.Fatalf("台帳 = TP %v(%v円)/ SL %v(%v円), want TP 3505(224)/ SL 3115(166)",
			p.TakeProfitPrice, p.TakeProfitJPY, p.StopLossPrice, p.StopLossJPY)
	}
}

// TP 0 は「利確脚を板に載せない」の意味で、台帳の利確まで消す指示ではない(消すと戦略の出口が変わる)。
func TestRepriceProtectiveOrder_KeepsLedgerTakeProfitWhenNoneGiven(t *testing.T) {
	repo, id, brk := repriceLedgerFixture(t)
	if _, err := newReprice(repo, brk, &fakeTripper{}).Execute(context.Background(),
		RepriceProtectiveInput{Symbol: "4901", StopLoss: 3115}); err != nil {
		t.Fatalf("err = %v", err)
	}
	p, _ := repo.GetByID(context.Background(), id)
	if p.TakeProfitPrice != 3721 || p.StopLossPrice != 3115 {
		t.Fatalf("台帳 = TP %v / SL %v, want TP 3721(据え置き)/ SL 3115", p.TakeProfitPrice, p.StopLossPrice)
	}
}

// 板の変更に失敗したら台帳は触らない(板と台帳を食い違わせない)。
func TestRepriceProtectiveOrder_LeavesLedgerAloneWhenPlacementFails(t *testing.T) {
	repo, id, brk := repriceLedgerFixture(t)
	brk.placeErr = errPlaceFailedForLedgerTest
	_, _ = newReprice(repo, brk, &fakeTripper{}).Execute(context.Background(),
		RepriceProtectiveInput{Symbol: "4901", TakeProfit: 3505, StopLoss: 3115})
	p, _ := repo.GetByID(context.Background(), id)
	if p.StopLossPrice != 2986.5 || p.TakeProfitPrice != 3721 {
		t.Fatalf("再発注に失敗したのに台帳が変わった: TP %v / SL %v", p.TakeProfitPrice, p.StopLossPrice)
	}
}

var errPlaceFailedForLedgerTest = errors.New("place failed")
