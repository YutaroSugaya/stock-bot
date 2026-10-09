package broker

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

func TestPaper_PlaceFillResolveClose(t *testing.T) {
	ctx := context.Background()
	c := clock.Fixed(time.Date(2026, 6, 17, 10, 0, 0, 0, time.UTC))
	p := NewPaper(c, 2, 0) // 2 ticks slippage
	p.SetPrice("7203", 2500)

	res, err := p.PlaceOrder(ctx, order.PlaceOrderRequest{Symbol: "7203", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if !res.Accepted || res.BrokerPositionID == "" {
		t.Fatalf("place not accepted: %+v", res)
	}

	// BUY fills with adverse slippage. 7203 は TOPIX500 = 細かい呼値なので
	// 2500 円での呼値は 0.5 円: 2500 + 2*0.5 = 2501(粗いテーブルなら 2502)。
	rex, err := p.ResolveExecution(ctx, res.OrderID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rex.FilledPrice != 2501 {
		t.Fatalf("fill price = %g, want 2501", rex.FilledPrice)
	}

	// OCO legs
	if _, err := p.PlaceSettleOCO(ctx, port.OCOCloseOrderInput{BrokerPositionID: res.BrokerPositionID, TakeProfit: 2600, StopLoss: 2450}); err != nil {
		t.Fatalf("oco: %v", err)
	}
	tp, sl, err := p.ResolveSettleLegs(ctx, res.BrokerPositionID, "7203")
	if err != nil || tp == "" || sl == "" {
		t.Fatalf("resolve legs: %v tp=%q sl=%q", err, tp, sl)
	}

	positions, _ := p.GetPositions(ctx)
	if len(positions) != 1 {
		t.Fatalf("want 1 open position, got %d", len(positions))
	}

	cr, err := p.ClosePosition(ctx, port.CloseRequest{Symbol: "7203", BrokerPositionID: res.BrokerPositionID, Side: order.SideSell, Quantity: 100})
	if err != nil || !cr.Accepted {
		t.Fatalf("close: %v accepted=%v", err, cr.Accepted)
	}
	positions, _ = p.GetPositions(ctx)
	if len(positions) != 0 {
		t.Fatalf("want 0 open positions after close, got %d", len(positions))
	}
}

func TestPaper_RejectsInvalidSide(t *testing.T) {
	p := NewPaper(nil, 0, 0)
	p.SetPrice("7203", 2500)
	if _, err := p.PlaceOrder(context.Background(), order.PlaceOrderRequest{Symbol: "7203", Side: order.Side("X"), Quantity: 100}); err == nil {
		t.Fatal("expected error for invalid side")
	}
}

// 🛑 paper は信用注文でも余力を返す。返さないと collateral ゲートが
// insufficient_margin で全エントリーを落とし、**research の forward 収集が
// 丸ごと止まる**(信用の建余力チェックを足したときの回帰)。
// 紙に「信用の建余力」という概念は無いので現物余力と同値でよい。
func TestPaperReportsMarginCapacity(t *testing.T) {
	pb := NewPaper(nil, 0, 0)
	pb.BalanceJPY = 500000
	am, err := pb.GetAccountMargin(context.Background())
	if err != nil {
		t.Fatalf("GetAccountMargin: %v", err)
	}
	if am.MarginNewJPY != 500000 {
		t.Errorf("MarginNewJPY=%v, want 500000(現物余力と同値)", am.MarginNewJPY)
	}
	if am.MarginCall {
		t.Error("paper で追証が立っている")
	}
}

// 🚨 価格を知らない銘柄の決済に**値段を捏造しない**。fillPrice は
// 未登録の価格 0 にスリッページを足していたので、売建の返済(買い)が 2 円で「確定約定」
// (FilledPrice > 0)し、建値 3,000 の空売りに +30 万円の架空勝ちが研究台帳へ載っていた
// (再起動直後に CLOSING を再発行する経路で到達する)。FilledPrice=0 で返し、呼び手の
// 観測価格 fallback / defer に任せる(TestCloseOne_PaperWithoutPrice_KeepsLegacyObservedPricePath)。
func TestPaper_ClosePositionWithoutPriceDoesNotFabricateAFill(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	for _, side := range []order.Side{order.SideSell, order.SideBuy} {
		p := NewPaper(clock.Fixed(now), 2, 0) // slippage 2 ticks(hard_limits と同じ)
		p.AdoptOpenPositions([]port.BrokerPosition{{
			BrokerPositionID: "bp-adopted", Symbol: "9999", Side: side, Quantity: 100, EntryPrice: 3000,
		}})
		res, err := p.ClosePosition(ctx, port.CloseRequest{Symbol: "9999", BrokerPositionID: "bp-adopted", Side: side.Opposite(), Quantity: 100})
		if err != nil {
			t.Fatal(err)
		}
		if res.FilledPrice != 0 {
			t.Fatalf("%s: 価格を知らないのに %v で約定した(スリッページ分の捏造)", side, res.FilledPrice)
		}
	}
}
