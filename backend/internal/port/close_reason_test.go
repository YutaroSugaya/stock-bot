package port

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
)

// `close_reason='ratchet_takeprofit'` が**損失で出ていた**。段2(LLM)も台帳も「利確が 3 回発火した」と
// 読む。gross の符号でラベルを分ける。
//
// 🛑 「決済値 vs 建値」の価格比較で分類しない — SELL 建玉が入るので空売りで逆になる。
// 側に依らない **gross(fee/carry 前)の符号**で判定する。
func TestRatchetCloseReasonSplitsOnGrossSign(t *testing.T) {
	cases := []struct {
		gross float64
		want  string
	}{
		{+12000, CloseReasonRatchetTakeProfit},
		{0, CloseReasonRatchetTakeProfit}, // gross ≥ 0 が本来の「利確」
		{-173, CloseReasonRatchetGivebackLoss},
		{-5856, CloseReasonRatchetGivebackLoss},
	}
	for _, c := range cases {
		if got := RatchetCloseReason(CloseReasonRatchetTakeProfit, c.gross); got != c.want {
			t.Errorf("RatchetCloseReason(ratchet, gross=%v) = %q, want %q", c.gross, got, c.want)
		}
	}
}

// ratchet 以外の理由には一切触らない(max_hold の損失を利確損に化けさせない)。
func TestRatchetCloseReasonLeavesOtherReasonsAlone(t *testing.T) {
	for _, r := range []string{
		"take_profit", "stop_loss", "max_hold", "early_exit", "manual",
		"reconcile_cold_close", "broker_close", "forced_flat",
		CloseReasonEntryCompensated, CloseReasonExternalClose, CloseReasonHarvestExpiry,
	} {
		if got := RatchetCloseReason(r, -9999); got != r {
			t.Errorf("RatchetCloseReason(%q, 損失) = %q — ratchet 以外は素通しすること", r, got)
		}
	}
}

// 🛑 **どちらも戦略の出口**なのでエッジ標本に入れる(entry_compensated /
// external_close とは扱いが違う)。片方だけ落ちると採点が静かに歪む。
func TestRatchetGivebackLossIsAStrategyClose(t *testing.T) {
	for _, r := range []string{CloseReasonRatchetTakeProfit, CloseReasonRatchetGivebackLoss} {
		if IsNonStrategyClose(r) {
			t.Errorf("%q が戦略外の決済に分類されている — エッジ標本から落ちる", r)
		}
		if !CountsTowardEntryGates(r) {
			t.Errorf("%q が再入場ゲートから外れている", r)
		}
	}
}

// domain が実際に出す理由文字列と port の定数がずれていないこと。文字列を 2 箇所に
// 書いている以上、ずれても両方緑で通ってしまう。
func TestPortConstantMatchesTheReasonDomainProduces(t *testing.T) {
	p := position.Position{
		Side: order.SideBuy, Quantity: 100, EntryPrice: 2000,
		RatchetArmJPY: 100, RatchetGivebackJPY: 150,
		PeakUnrealizedJPY: 200, RatchetArmed: true, RatchetFloorAtArm: true,
		Status: position.StatusOpen,
	}
	d := position.EvaluateExit(p, p.PriceForUnrealized(100), time.Now())
	if !d.Exit {
		t.Fatal("前提: 床に触れて決済すること")
	}
	if d.Reason != CloseReasonRatchetTakeProfit {
		t.Fatalf("domain の理由 %q と port の定数 %q がずれている", d.Reason, CloseReasonRatchetTakeProfit)
	}
}

// harvest の打ち切り(migration 0018)は **max_hold と別の理由**でなければならない。
// 同じにすると台帳で「戦略が決めた期限で落ちた玉」と「人間が後から打ち切った玉」が
// 混ざり、エッジ判定と保有期間分布が検閲標本を自然決済として数える(打ち切りを
// 自然決済として数える壊れ方)。
func TestHarvestExpiryIsItsOwnReason(t *testing.T) {
	if CloseReasonHarvestExpiry != "harvest_expiry" {
		t.Fatalf("値は migration 0018 の trades_close_reason_check と一致すること: %q", CloseReasonHarvestExpiry)
	}
	for _, other := range []string{"max_hold", "forced_flat", "manual"} {
		if CloseReasonHarvestExpiry == other {
			t.Fatalf("harvest の打ち切りを %q と同じ理由で記録してはいけない", other)
		}
	}
	// 🛑 **戦略の出口としては数える**(forced_flat / max_hold と同じ扱い)。検閲で
	// あることは理由の名前と cmd/counterfactual で読む — エッジ標本から丸ごと落とすと、
	// 「打ち切った建玉は無かったこと」になり、
	// 「残玉は検閲標本として台帳に記録」という規則と食い違う。
	if IsNonStrategyClose(CloseReasonHarvestExpiry) {
		t.Error("harvest_expiry をエッジ標本から落としてはいけない(検閲は理由の名前で読む)")
	}
	if !CountsTowardEntryGates(CloseReasonHarvestExpiry) {
		t.Error("harvest_expiry が再入場ゲートから外れている")
	}
}

// SQL readers (pg.PairRepo) exclude the same set the Go predicate excludes — the
// slice is the single source both consume.
func TestNonStrategyCloseReasonsBacksIsNonStrategyClose(t *testing.T) {
	rs := NonStrategyCloseReasons()
	if len(rs) != 3 {
		t.Fatalf("NonStrategyCloseReasons = %v, want the 3 non-strategy closes", rs)
	}
	for _, r := range rs {
		if !IsNonStrategyClose(r) {
			t.Fatalf("%q is listed but IsNonStrategyClose says false", r)
		}
	}
	for _, r := range []string{"take_profit", "stop_loss", CloseReasonRatchetGivebackLoss, CloseReasonHarvestExpiry} {
		if IsNonStrategyClose(r) {
			t.Fatalf("%q is a strategy close", r)
		}
	}
}

// 分割の誤読で決済した往復(migration 0022)は**戦略の出口ではない** — エッジ標本から外す。
// 再入場ゲートには数える(bot 自身の行動で、entry_compensated と同じ側)。
func TestSplitMisfireIsNotAStrategyClose(t *testing.T) {
	if CloseReasonSplitMisfire != "split_misfire" {
		t.Fatalf("定数 %q が migration 0022 の CHECK 値とずれている", CloseReasonSplitMisfire)
	}
	if !IsNonStrategyClose(CloseReasonSplitMisfire) {
		t.Fatal("split_misfire がエッジ標本に入る — 分割の誤読は戦略の出口ではない")
	}
	if !CountsTowardEntryGates(CloseReasonSplitMisfire) {
		t.Fatal("split_misfire が再入場ゲートから外れている(bot 自身の行動)")
	}
	if got := RatchetCloseReason(CloseReasonSplitMisfire, -1); got != CloseReasonSplitMisfire {
		t.Fatalf("RatchetCloseReason が split_misfire を %q に書き換えた", got)
	}
}
