package command

// Test doubles shared by the protective-order command tests.

import (
	"context"
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
)

// MOCK rationale (TESTING.md §1 system boundary): broker は外部境界。
type fakeExpiryBroker struct {
	orders    map[string][]port.ProtectiveOrderInfo
	listErr   error
	cancelled []string
	cancelErr map[string]error
	// heldSymbols は **broker 側に建玉が在る銘柄**。nil は「建玉照会が落ちた =
	// 確かめられない」を意味する。
	//
	// 🛑 **nil を空集合(= 何も持っていない)と読ませない。**「守りが無い」と
	// 「建玉が無い」は正反対で、既定で後者に倒すと、建玉照会を語っていない
	// テストが全部「決済済みなので何もしない」で緑になる。
	heldSymbols  []string
	positionsErr error
	// heldQty / heldEntry は銘柄ごとの broker 側の株数・建単価(未設定なら 100 株・0)。
	// 株式分割で broker だけが建玉を言い直した状態を作るために使う。
	heldQty   map[string]int
	heldEntry map[string]float64
}

func (f *fakeExpiryBroker) GetPositions(_ context.Context) ([]port.BrokerPosition, error) {
	if f.positionsErr != nil {
		return nil, f.positionsErr
	}
	if f.heldSymbols == nil {
		return nil, errors.New("fake: 建玉照会は配線されていない(heldSymbols 未設定)")
	}
	out := make([]port.BrokerPosition, 0, len(f.heldSymbols))
	for _, s := range f.heldSymbols {
		qty := 100
		if q, ok := f.heldQty[s]; ok {
			qty = q
		}
		out = append(out, port.BrokerPosition{
			BrokerPositionID: "b-" + s, Symbol: s, Side: order.SideBuy, Quantity: qty,
			EntryPrice: f.heldEntry[s], ExecKind: order.ExecMarginSystem,
		})
	}
	return out, nil
}

func (f *fakeExpiryBroker) CancelProtectiveOrder(_ context.Context, o port.ProtectiveOrderInfo) error {
	if err := f.cancelErr[o.OrderID]; err != nil {
		return err
	}
	f.cancelled = append(f.cancelled, o.OrderID)
	return nil
}

func (f *fakeExpiryBroker) ListProtectiveOrders(_ context.Context, symbol string) ([]port.ProtectiveOrderInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.orders[symbol], nil
}

func testHours() session.TradingHours {
	return session.TradingHours{TZ: clock.JST, CalendarThrough: time.Date(2026, 12, 31, 0, 0, 0, 0, clock.JST)}
}

func seedMultiday(t *testing.T, repo *repository.InMemoryPositionRepo, sym string, openedAt time.Time) int64 {
	t.Helper()
	id, err := repo.Insert(context.Background(), port.PositionInsertInput{
		BrokerPositionID: "b-" + sym, Symbol: sym, Side: order.SideBuy, Quantity: 100,
		OpenedAt: openedAt, HoldingMode: order.HoldingMultiday, MaxHoldMinutes: 14400,
		// 🛑 **live の建玉と同じ制度信用**。ここを空(= 現物)にしていると
		// 「守りを現物区分で出し直す」バグをテストが素通りさせる(本番では
		// 建玉が裸になる)。fixture が本番と違う区分を持つと、区分の取り違えは
		// 原理的に検出できない。
		ExecKind: order.ExecMarginSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
