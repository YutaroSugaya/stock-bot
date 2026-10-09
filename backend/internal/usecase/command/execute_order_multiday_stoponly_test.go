package command

import (
	"context"
	"testing"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// **多日建玉の板の守りは stop-only**。TP 脚は帯の内側でも
// 板に載せず、台帳の take_profit_price に残して OnTick が持つ。
//
// 根拠: 立花は期日付きの返済注文を毎営業日に翌日へ繰り越すとき翌日の値幅制限で再検査し、
// TP 脚が帯の外だと **SL 脚ごと失効させる**(live で複数回観測)。
// 急落した翌朝ほど SL が消える構造なので、多日の TP を板に載せない。
func TestExecuteOrder_MultidayPlacesStopOnlyEvenWhenTakeProfitIsInsideTheBand(t *testing.T) {
	ctx := context.Background()
	var spy *ocoSpyBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		spy = &ocoSpyBroker{LiveBroker: pb}
		return spy
	})

	f.paper.SetPrice("7203", 5381)
	sig := entrySignal(f.now)
	sig.HoldingMode = order.HoldingMultiday
	sig.Quantity = 100
	sig.EntryPrice = 5381
	sig.TakeProfitJPY = 500 // → TP 5,881(帯 4,352〜6,352 の内側)
	sig.StopLossJPY = 583   // → SL 4,798
	id, err := f.exec.Execute(ctx, ExecuteOrderInput{
		Signal: sig, Quantity: 100, ExecKind: order.ExecMarginSystem,
		Source: position.SourceBot, PriceLimitRef: 5352,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if spy.got.TakeProfit != 0 {
		t.Fatalf("多日建玉なのに TP 脚 %v を板に載せた — 翌朝の繰越で帯の外に出ると SL ごと失効する", spy.got.TakeProfit)
	}
	if spy.got.StopLoss <= 0 {
		t.Fatalf("SL 脚が無い(%v)", spy.got.StopLoss)
	}
	p, err := f.posRepo.GetByID(ctx, id)
	if err != nil || p == nil {
		t.Fatalf("GetByID: %v (p=%v)", err, p)
	}
	if p.TakeProfitPrice <= 0 {
		t.Fatalf("台帳の take_profit_price = %v — OnTick が引き継ぐ値が無い", p.TakeProfitPrice)
	}
}

// intraday は当日限り(繰越が無い)なので従来どおり両脚を載せる。
func TestExecuteOrder_IntradayStillPlacesBothLegsInsideTheBand(t *testing.T) {
	ctx := context.Background()
	var spy *ocoSpyBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		spy = &ocoSpyBroker{LiveBroker: pb}
		return spy
	})
	f.paper.SetPrice("7203", 5381)
	sig := entrySignal(f.now) // HoldingIntraday
	sig.Quantity = 100
	sig.EntryPrice = 5381
	sig.TakeProfitJPY = 500
	sig.StopLossJPY = 583
	if _, err := f.exec.Execute(ctx, ExecuteOrderInput{
		Signal: sig, Quantity: 100, ExecKind: order.ExecMarginGeneral,
		Source: position.SourceBot, PriceLimitRef: 5352,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if spy.got.TakeProfit <= 0 {
		t.Fatalf("intraday の帯内 TP を落とした(%v)", spy.got.TakeProfit)
	}
}
