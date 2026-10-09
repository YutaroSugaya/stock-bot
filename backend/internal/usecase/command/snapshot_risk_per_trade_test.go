package command

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

// 1本あたりの計画損失の上限は **BuildStructural**(口座照会の前)で載る。
//
// 🛑 FillCollateral 側に載せると、判定材料が Signal だけで足りるのに立花へ
// wire 3 リクエストを払ってから落とすことになる(API 予算を焼いた形)。
// gate も EvaluateStructural に置いてあるので、ここがズレると
// 「cap を設定したのに一度も効かない」= 一番気づきにくい壊れ方になる。
func riskCapSnapshot(t *testing.T, cap int) (snapWith, snapWithout int) {
	t.Helper()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	c := clock.Fixed(now)

	build := func(capJPY int) int {
		caps := SnapshotCaps{RequiredMarginRate: 0.30, WindowMinutes: 60, MaxRiskPerTradeJPY: capJPY}
		sb := NewSnapshotBuilder(h.posRepo, h.tradeRepo, h.broker, h.emergency, h.hours, c, caps)
		sig := strategy.Signal{
			Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "7203",
			EntryPrice: 2500, StopLossJPY: 50, Quantity: 100, HoldingMode: order.HoldingMultiday,
		}
		snap := sb.BuildStructural(context.Background(), "7203",
			&market.MarketSummary{}, sig, order.ExecMarginSystem)
		return snap.MaxRiskPerTradeJPY
	}
	return build(cap), build(0)
}

func TestBuildStructural_CarriesMaxRiskPerTradeCap(t *testing.T) {
	with, _ := riskCapSnapshot(t, 35000)
	if with != 35000 {
		t.Fatalf("構造スナップショットに上限が載っていない: %d", with)
	}
}

// 未設定は 0 = 無効のまま素通し(research / harvest の標本を censoring しない)。
func TestBuildStructural_MaxRiskPerTradeDefaultsToDisabled(t *testing.T) {
	_, without := riskCapSnapshot(t, 35000)
	if without != 0 {
		t.Fatalf("未設定なのに上限が立った: %d", without)
	}
}
