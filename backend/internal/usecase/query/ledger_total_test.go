package query

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 累計(決済 + 含み)はダッシュボードの累計欄に出す値。
// 決済の net は台帳と同じ規約(gross − fee + carry。carry は負で入る)。含みは時価がある建玉だけ入れ、
// 無い銘柄は名指しで返す(黙って 0 にすると「戻った」に見える)。

func TestLedgerTotal_SumsRealizedAndUnrealized(t *testing.T) {
	ctx := context.Background()
	trades := repository.NewInMemoryTradeRepo()
	for _, tr := range []port.TradeRecord{
		{Symbol: "1111", Side: order.SideBuy, Quantity: 100, ProfitLossJPY: 10_000, FeeJPY: 0, CarryJPY: -500, ClosedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{Symbol: "2222", Side: order.SideBuy, Quantity: 100, ProfitLossJPY: -30_000, FeeJPY: 100, CarryJPY: -200, ClosedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
	} {
		if _, err := trades.Insert(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	pos := repository.NewInMemoryPositionRepo()
	for _, in := range []port.PositionInsertInput{
		{Symbol: "3333", Side: order.SideBuy, Quantity: 100, EntryPrice: 2_000, StrategyConfigID: "c", HoldingMode: order.HoldingMultiday},
		{Symbol: "4444", Side: order.SideSell, Quantity: 100, EntryPrice: 1_000, StrategyConfigID: "c", HoldingMode: order.HoldingMultiday},
		{Symbol: "5555", Side: order.SideBuy, Quantity: 100, EntryPrice: 500, StrategyConfigID: "c", HoldingMode: order.HoldingMultiday},
	} {
		if _, err := pos.Insert(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	last := map[string]float64{"3333": 2_100, "4444": 1_050} // 5555 は時価なし
	q := NewLedgerTotal(trades, pos)
	v, err := q.Execute(ctx, func(sym string) (float64, bool) {
		px, ok := last[sym]
		return px, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	// 決済: (10,000 − 0 − 500) + (−30,000 − 100 − 200) = −20,800
	if v.RealizedNetJPY != -20_800 || v.ClosedN != 2 {
		t.Fatalf("realized = %+v", v)
	}
	// 含み: BUY 3333 (2,100 − 2,000) × 100 = +10,000 / SELL 4444 (1,000 − 1,050) × 100 = −5,000 / 5555 は入れない
	if v.UnrealizedJPY != 5_000 || v.OpenN != 3 || v.PricedN != 2 {
		t.Fatalf("unrealized = %+v", v)
	}
	if len(v.Unpriced) != 1 || v.Unpriced[0] != "5555" {
		t.Fatalf("時価の無い銘柄を名指ししていない: %+v", v.Unpriced)
	}
	if v.TotalJPY != -15_800 {
		t.Fatalf("total = %v", v.TotalJPY)
	}
}

func TestLedgerTotal_EmptyLedgerIsZero(t *testing.T) {
	q := NewLedgerTotal(repository.NewInMemoryTradeRepo(), repository.NewInMemoryPositionRepo())
	v, err := q.Execute(context.Background(), func(string) (float64, bool) { return 0, false })
	if err != nil {
		t.Fatal(err)
	}
	if v.TotalJPY != 0 || v.ClosedN != 0 || v.OpenN != 0 || len(v.Unpriced) != 0 {
		t.Fatalf("empty = %+v", v)
	}
}
