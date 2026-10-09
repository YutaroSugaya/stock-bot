package config

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/session"
)

// bot と backtest は同じ hard_limits から同じ carry を組み立てる(D-12)。料率は % → 小数。
func TestHardLimitsCarryCalc(t *testing.T) {
	hl, err := LoadHardLimits("../../../configs/hard_limits.yaml")
	if err != nil {
		t.Fatal(err)
	}
	hours, err := hl.SessionHours.TradingHours()
	if err != nil {
		t.Fatal(err)
	}
	c := hl.CarryCalc(hours)
	if c.Rates.BuyAnnualRate != hl.Margin.BuyAnnualRatePct/100 || c.Rates.SellAnnualRate != hl.Margin.SellLendingAnnualPct/100 {
		t.Fatalf("料率 = %+v", c.Rates)
	}
	if c.SettlementDays != 2 || c.IsTradingDay == nil || c.TZ != hours.TZ {
		t.Fatalf("受渡 / 暦 = %+v", c)
	}
	p := position.Position{Side: order.SideBuy, Quantity: 100, EntryPrice: 2000, ExecKind: order.ExecMarginSystem,
		OpenedAt: time.Date(2026, 9, 18, 10, 0, 0, 0, clock.JST)}
	if got := c.JPY(p, time.Date(2026, 9, 25, 10, 0, 0, 0, clock.JST)); got >= 0 {
		t.Fatalf("信用買いの carry = %v, want 負", got)
	}
}

// 受渡日数が未設定でも受渡日を今日扱いにしない(日本株 T+2)。
func TestHardLimitsCarryCalc_DefaultsSettlementToT2(t *testing.T) {
	var hl HardLimits
	if got := hl.CarryCalc(session.TradingHours{TZ: clock.JST}).SettlementDays; got != 2 {
		t.Fatalf("SettlementDays = %d, want 2", got)
	}
}
