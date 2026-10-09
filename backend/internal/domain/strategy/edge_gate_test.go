package strategy

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
)

// 🚨 見送りシグナルは長らく **銘柄を空のまま**返していた。誰も困らなかったのは、
// 見送りがどこにも記録されていなかったから。`tp_below_cost_floor` を
// signal_rejections に書き始めた瞬間に、**銘柄の無い行しか作れない**ことが判明した。
// アーム別に数えられない行は、書いても検出器として機能しない。
func TestCostFloorNoTradeCarriesTheSymbol(t *testing.T) {
	cfg := &config.StrategyConfig{ConfigID: "cfg-1", Symbol: "7203"}
	in := EvalInput{Config: cfg}
	// TP が床(スプレッド 5 + スリッページ 1 tick = 6 tick × 1円)に遠く届かない。
	floor := CostFloor{SpreadTicks: 5, SlippageTicks: 1, MinEdgeMultiple: 1, TickSize: 1}
	sig := applyCostFloor(in, Signal{
		Decision: DecisionEnter, Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		TakeProfitJPY: 1, HoldingMode: order.HoldingIntraday,
		StrategyName: config.StrategyAbsMomentumV2,
	}, floor)

	if sig.Decision != DecisionNoTrade || sig.Reason != ReasonTPBelowCostFloor {
		t.Fatalf("床に落ちていない: decision=%v reason=%q", sig.Decision, sig.Reason)
	}
	if sig.Symbol != "7203" {
		t.Errorf("Symbol = %q, want 7203 — signal_rejections に銘柄の無い行が書かれ、"+
			"アーム別の件数が数えられない", sig.Symbol)
	}
	if sig.StrategyName != config.StrategyAbsMomentumV2 {
		t.Errorf("StrategyName = %q — どのアームが落ちたか判らない", sig.StrategyName)
	}
	if sig.ConfigID != "cfg-1" {
		t.Errorf("ConfigID = %q — 行が config に紐づかない", sig.ConfigID)
	}
}

// 床を越える提案は素通し(門を足したせいで全部落ちる、を防ぐ)。
func TestCostFloorPassesAnEntryThatClearsIt(t *testing.T) {
	in := EvalInput{Config: &config.StrategyConfig{ConfigID: "cfg-1", Symbol: "7203"}}
	floor := CostFloor{SpreadTicks: 5, SlippageTicks: 1, MinEdgeMultiple: 1, TickSize: 1}
	sig := applyCostFloor(in, Signal{
		Decision: DecisionEnter, Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		TakeProfitJPY: 100, HoldingMode: order.HoldingIntraday,
	}, floor)
	if sig.Decision != DecisionEnter {
		t.Fatalf("床を越えているのに落ちた: %q", sig.Reason)
	}
}

// 🛑 uncapped な trail(TP=0)は **ratchet arm 距離**を目標として床に当てる。
// 0 と比べると trail アームが 1 本も建たない。
func TestCostFloorUsesTheRatchetArmForUncappedTrails(t *testing.T) {
	in := EvalInput{Config: &config.StrategyConfig{ConfigID: "cfg-1", Symbol: "7203"}}
	floor := CostFloor{SpreadTicks: 5, SlippageTicks: 1, MinEdgeMultiple: 1, TickSize: 1}
	trail := Signal{
		Decision: DecisionEnter, Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		TakeProfitJPY: 0, RatchetArmJPY: 100, HoldingMode: order.HoldingIntraday,
	}
	if got := applyCostFloor(in, trail, floor); got.Decision != DecisionEnter {
		t.Fatalf("TP=0 の trail が床で落ちた(0 と比較している): %q", got.Reason)
	}
}
