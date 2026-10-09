package position

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
)

func TestTPSLPricesFromJPY_Buy(t *testing.T) {
	// 7203 は TOPIX500・2500円 → 呼値 0.5。+20円 / -10円。
	tp, sl := TPSLPricesFromJPY("7203", order.SideBuy, 2500, 20, 10)
	if tp != 2520 || sl != 2490 {
		t.Fatalf("buy tp/sl = %g/%g, want 2520/2490", tp, sl)
	}
}

func TestTPSLPricesFromJPY_Sell(t *testing.T) {
	tp, sl := TPSLPricesFromJPY("7203", order.SideSell, 2500, 20, 10)
	if tp != 2480 || sl != 2510 {
		t.Fatalf("sell tp/sl = %g/%g, want 2480/2510", tp, sl)
	}
}

// 呼値に乗らない幅は発注できる値段へ丸める(唯一 tick が要る場所)。
func TestTPSLPricesFromJPY_RoundsToTickGrid(t *testing.T) {
	// ユニバース外(粗い呼値): 4000円 → 呼値 5。+3円 は 4005 ではなく 4000 へ丸まる。
	tp, sl := TPSLPricesFromJPY("9999", order.SideBuy, 4000, 3, 3)
	if int(tp)%5 != 0 || int(sl)%5 != 0 {
		t.Fatalf("tp/sl not tick-aligned: %g/%g", tp, sl)
	}
}

func TestMaxHoldUntil(t *testing.T) {
	opened := time.Date(2026, 6, 17, 9, 30, 0, 0, time.UTC)
	p := Position{OpenedAt: opened, MaxHoldMinutes: 60}
	if got := p.MaxHoldUntil(); !got.Equal(opened.Add(time.Hour)) {
		t.Fatalf("MaxHoldUntil = %v, want %v", got, opened.Add(time.Hour))
	}
	if got := (Position{OpenedAt: opened}).MaxHoldUntil(); !got.IsZero() {
		t.Fatalf("MaxHoldUntil with no max = %v, want zero", got)
	}
}
