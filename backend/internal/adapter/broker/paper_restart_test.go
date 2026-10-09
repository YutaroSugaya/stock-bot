package broker_test

import (
	"context"
	"testing"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 紙の建玉帳は プロセス内 map なので再起動で消えるが、Postgres の positions は
// OPEN のまま残る。再起動後に TP/SL が来たとき ClosePosition が "position not
// found" を返すと、守りを外した直後の reject 扱いで emergency trip し、行は
// CLOSING のまま座礁する(paper には reconcile ループが無い)。
// → 起動時に台帳から紙の帳簿を復元する。
func TestPaperAdoptOpenPositionsSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	old := broker.NewPaper(clock.System(), 0, 0)
	old.SetPrice("7203", 3000)
	res, err := old.PlaceOrder(ctx, order.PlaceOrderRequest{
		Symbol: "7203", Side: order.SideBuy, Quantity: 100, Type: order.OrderTypeMarket})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	bpID := res.BrokerPositionID

	// --- 再起動(プロセス入れ替え。台帳=Postgres は残っている) ---
	fresh := broker.NewPaper(clock.System(), 0, 0)
	fresh.SetPrice("7203", 3100)
	n := fresh.AdoptOpenPositions([]port.BrokerPosition{{
		BrokerPositionID: bpID, Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 3000,
	}})
	if n != 1 {
		t.Fatalf("AdoptOpenPositions = %d, want 1", n)
	}
	got, err := fresh.ClosePosition(ctx, port.CloseRequest{
		Symbol: "7203", BrokerPositionID: bpID, Side: order.SideSell, Quantity: 100})
	if err != nil {
		t.Fatalf("ClosePosition: %v", err)
	}
	if !got.Accepted {
		t.Fatalf("再起動後に建玉を決済できない(=TP/SL が死ぬ): %+v", got)
	}
	if got.FilledPrice != 3100 {
		t.Fatalf("FilledPrice = %v, want 3100 (復元後の実勢で約定)", got.FilledPrice)
	}
}

// 採番は毎プロセス 1 から始まるので、復元しないと新しい建玉が既存の
// broker_position_id を再利用する(bp-2 が別銘柄に化ける = 台帳が別銘柄の値段で
// 記録される)。復元した id より必ず先へ進めること。
func TestPaperAdoptAdvancesIDSequence(t *testing.T) {
	ctx := context.Background()
	fresh := broker.NewPaper(clock.System(), 0, 0)
	fresh.SetPrice("6758", 2000)
	fresh.AdoptOpenPositions([]port.BrokerPosition{
		{BrokerPositionID: "bp-2", Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 3000},
		{BrokerPositionID: "bp-6", Symbol: "8306", Side: order.SideBuy, Quantity: 100, EntryPrice: 900},
	})
	res, err := fresh.PlaceOrder(ctx, order.PlaceOrderRequest{
		Symbol: "6758", Side: order.SideBuy, Quantity: 100, Type: order.OrderTypeMarket})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	for _, taken := range []string{"bp-2", "bp-6"} {
		if res.BrokerPositionID == taken {
			t.Fatalf("新規建玉が既存 id %q を再利用した(別銘柄の建玉を決済してしまう)", taken)
		}
	}
	// 復元した建玉は新規発注で壊れない。
	if got, err := fresh.ClosePosition(ctx, port.CloseRequest{
		Symbol: "7203", BrokerPositionID: "bp-2", Side: order.SideSell, Quantity: 100}); err != nil || !got.Accepted {
		t.Fatalf("復元建玉 bp-2 が決済できない: %+v %v", got, err)
	}
}

// 冪等: 同じ建玉を二重に復元しても帳簿は 1 本のまま。
func TestPaperAdoptIsIdempotent(t *testing.T) {
	fresh := broker.NewPaper(clock.System(), 0, 0)
	in := []port.BrokerPosition{{BrokerPositionID: "bp-2", Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 3000}}
	fresh.AdoptOpenPositions(in)
	if n := fresh.AdoptOpenPositions(in); n != 0 {
		t.Fatalf("2 回目の復元で %d 件追加された(冪等でない)", n)
	}
	ps, err := fresh.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("帳簿 = %d 本, want 1", len(ps))
	}
}
