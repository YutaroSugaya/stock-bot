package backtest

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/session"
)

func prodCarry() position.CarryCalc {
	hours := session.TradingHours{TZ: clock.JST, Holidays: map[string]struct{}{"2026-09-22": {}}}
	return position.CarryCalc{
		Rates:          position.MarginRates{BuyAnnualRate: 0.025, SellAnnualRate: 0.0115},
		TZ:             clock.JST,
		IsTradingDay:   hours.IsTradingDay,
		SettlementDays: 2,
	}
}

// 🛑 backtest の carry は**本番と同じモデル**(position.CarryCalc: 約定金額 × 年率 × 受渡日の両端入れ)。
// 別のモデル(100 株あたり × 営業日)を持つと、同じ往復に backtest と forward が別の net を付ける(D-12)。
func TestCarryIsTheProductionModel(t *testing.T) {
	m := CostModel{Carry: prodCarry()}
	p := position.Position{
		Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 2000,
		ExecKind: order.ExecMarginSystem, HoldingMode: order.HoldingMultiday,
		OpenedAt: time.Date(2026, 9, 18, 10, 0, 0, 0, clock.JST),
	}
	closed := time.Date(2026, 9, 25, 10, 0, 0, 0, clock.JST)
	want := prodCarry().JPY(p, closed)
	if want >= 0 {
		t.Fatalf("前提: 本番の carry が負でない(%v)", want)
	}
	if got := m.CarryJPY(p, closed); got != want {
		t.Fatalf("backtest carry = %v, want 本番と同じ %v", got, want)
	}
	// 現物と執行区分の無い建玉(料率を捏造しない)は 0。
	p.ExecKind = order.ExecCash
	if got := m.CarryJPY(p, closed); got != 0 {
		t.Fatalf("現物の carry = %v, want 0", got)
	}
	p.ExecKind = ""
	if got := m.CarryJPY(p, closed); got != 0 {
		t.Fatalf("執行区分なしの carry = %v, want 0", got)
	}
}
