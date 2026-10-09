package position_test

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
)

var jst = clock.JST

// 平日のみ営業日(祝日なし)の素朴なカレンダー。
func weekdaysOnly(t time.Time) bool {
	wd := t.In(jst).Weekday()
	return wd != time.Saturday && wd != time.Sunday
}

func carryCalc() position.CarryCalc {
	return position.CarryCalc{
		Rates:          position.MarginRates{BuyAnnualRate: 0.025, SellAnnualRate: 0.0115},
		TZ:             jst,
		IsTradingDay:   weekdaysOnly,
		SettlementDays: 2,
	}
}

func at(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, jst) }

// 立花証券 e支店の一般信用(買い)は年 2.50%。約定代金ベース・受渡日の両端入れ・日計りは 1 日分。
func TestCarryDayTradeChargesOneDay(t *testing.T) {
	p := position.Position{
		Side: order.SideBuy, Quantity: 100, EntryPrice: 3000,
		ExecKind: order.ExecMarginGeneral, OpenedAt: at(2026, 7, 27, 10),
	}
	got := carryCalc().JPY(p, at(2026, 7, 27, 14))
	// 300,000 × 2.5% ÷ 365 × 1日 = 20.547…
	want := -300000 * 0.025 / 365 * 1
	if diff := got - want; diff > 0.001 || diff < -0.001 {
		t.Fatalf("carry = %v, want %v (日計り 1 日分)", got, want)
	}
	if got >= 0 {
		t.Fatalf("carry はコストなので負であるべき: %v", got)
	}
}

// 金曜建て→月曜決済。受渡は T+2 営業日(火 7/28 → 水 7/29)で両端入れ 2 日分。土日は受渡日より前なので課金されない。
func TestCarryFridayToMondayChargesTwoDays(t *testing.T) {
	p := position.Position{
		Side: order.SideBuy, Quantity: 100, EntryPrice: 3865,
		ExecKind: order.ExecMarginGeneral, OpenedAt: at(2026, 7, 24, 13),
	}
	got := carryCalc().JPY(p, at(2026, 7, 27, 10))
	want := -386500 * 0.025 / 365 * 2
	if diff := got - want; diff > 0.001 || diff < -0.001 {
		t.Fatalf("carry = %v, want %v (受渡 7/28→7/29 の両端入れ 2 日)", got, want)
	}
}

// 売り建ては貸株料(年 1.15%)。
func TestCarryShortUsesStockLendingRate(t *testing.T) {
	p := position.Position{
		Side: order.SideSell, Quantity: 100, EntryPrice: 2000,
		ExecKind: order.ExecMarginGeneral, OpenedAt: at(2026, 7, 27, 10),
	}
	got := carryCalc().JPY(p, at(2026, 7, 27, 14))
	want := -200000 * 0.0115 / 365 * 1
	if diff := got - want; diff > 0.001 || diff < -0.001 {
		t.Fatalf("carry = %v, want %v (貸株料)", got, want)
	}
}

// 現物(cash)に金利は無い — 0 を返す(0 を「未実装」と混同させない)。
func TestCarryCashIsZero(t *testing.T) {
	p := position.Position{
		Side: order.SideBuy, Quantity: 100, EntryPrice: 3000,
		ExecKind: order.ExecCash, OpenedAt: at(2026, 7, 24, 10),
	}
	if got := carryCalc().JPY(p, at(2026, 7, 27, 10)); got != 0 {
		t.Fatalf("現物の carry = %v, want 0", got)
	}
}

// 料率未設定(ゼロ値)なら 0 — 勝手にレートを捏造しない。
func TestCarryZeroRateIsZero(t *testing.T) {
	c := carryCalc()
	c.Rates = position.MarginRates{}
	p := position.Position{
		Side: order.SideBuy, Quantity: 100, EntryPrice: 3000,
		ExecKind: order.ExecMarginGeneral, OpenedAt: at(2026, 7, 24, 10),
	}
	if got := c.JPY(p, at(2026, 7, 27, 10)); got != 0 {
		t.Fatalf("料率未設定の carry = %v, want 0", got)
	}
}

// カレンダーが全て非営業日を返しても(年次更新切れの fail-close)無限ループしない。
func TestCarryTerminatesWithStaleCalendar(t *testing.T) {
	c := carryCalc()
	c.IsTradingDay = func(time.Time) bool { return false }
	p := position.Position{
		Side: order.SideBuy, Quantity: 100, EntryPrice: 3000,
		ExecKind: order.ExecMarginGeneral, OpenedAt: at(2026, 7, 24, 10),
	}
	got := c.JPY(p, at(2026, 7, 27, 10))
	if got >= 0 {
		t.Fatalf("carry = %v, want 負(打ち切っても課金はする)", got)
	}
}

// 決済が建玉より前(時計のずれ・異常データ)でも最低 1 日分で、正にはしない。
func TestCarryNeverGoesPositive(t *testing.T) {
	p := position.Position{
		Side: order.SideBuy, Quantity: 100, EntryPrice: 3000,
		ExecKind: order.ExecMarginGeneral, OpenedAt: at(2026, 7, 27, 10),
	}
	got := carryCalc().JPY(p, at(2026, 7, 24, 10))
	want := -300000 * 0.025 / 365 * 1
	if diff := got - want; diff > 0.001 || diff < -0.001 {
		t.Fatalf("carry = %v, want %v (最低 1 日分)", got, want)
	}
}
