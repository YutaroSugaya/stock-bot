package risk

import (
	"testing"

	"stockbot/backend/internal/domain/order"
)

// **多日建玉の板の守りは stop-only**(TP は OnTick が持つ)。
//
// 根拠(live で複数回観測・反例ゼロ): 立花は期日付きの返済注文を毎営業日に翌日へ繰り越すとき
// 翌日の値幅制限で再検査し、**TP 脚が帯の外に出ていると SL 脚ごと失効させる**。
// ある朝、TP 3,305 > 帯上限 3,184 と TP 5,366 > 5,218 の 2 建玉の守りが消え、TP の無い
// 建玉と TP が帯内の建玉は残った。別の日にも同型。急落した翌朝ほど SL が
// 消える構造なので、多日の TP を板に載せない。

// 基準 5,352 → 帯 4,352〜6,352。TP 5,881 は帯の内側。
const (
	tpLegRef    = 5352.0
	tpLegInside = 5881.0
	tpLegOut    = 6435.0
)

func TestProtectiveTakeProfitOnBoard_MultidayIsStopOnlyEvenInsideTheBand(t *testing.T) {
	place, reason := ProtectiveTakeProfitOnBoard(order.HoldingMultiday, order.SideSell, tpLegInside, tpLegRef)
	if place {
		t.Fatal("多日建玉の TP を板に載せた — 翌朝の繰越で帯の外に出ると SL ごと失効する(2026-09-09)")
	}
	if reason != "multiday_stop_only" {
		t.Fatalf("reason = %q, want multiday_stop_only", reason)
	}
}

// 基準値段が無くても多日は載せない(判定材料の有無に依らない方針)。
func TestProtectiveTakeProfitOnBoard_MultidayIsStopOnlyWithoutReference(t *testing.T) {
	if place, _ := ProtectiveTakeProfitOnBoard(order.HoldingMultiday, order.SideSell, tpLegInside, 0); place {
		t.Fatal("基準値段が無いときに多日の TP を載せた")
	}
}

// holding が空(不明)の建玉も多日側に倒す — SL は必ず残り、失うのは TP の機会だけ。
func TestProtectiveTakeProfitOnBoard_UnknownHoldingFallsToStopOnly(t *testing.T) {
	if place, _ := ProtectiveTakeProfitOnBoard("", order.SideSell, tpLegInside, tpLegRef); place {
		t.Fatal("holding 不明の建玉に TP を載せた(fail-safe は stop-only 側)")
	}
}

// intraday は当日限りで繰越が無いので従来どおり: 帯の内側なら載せる。
func TestProtectiveTakeProfitOnBoard_IntradayKeepsTakeProfitInsideTheBand(t *testing.T) {
	place, reason := ProtectiveTakeProfitOnBoard(order.HoldingIntraday, order.SideSell, tpLegInside, tpLegRef)
	if !place || reason != "inside_price_limit" {
		t.Fatalf("intraday の帯内 TP を落とした: place=%v reason=%q", place, reason)
	}
}

func TestProtectiveTakeProfitOnBoard_IntradayDropsTakeProfitOutsideTheBand(t *testing.T) {
	place, reason := ProtectiveTakeProfitOnBoard(order.HoldingIntraday, order.SideSell, tpLegOut, tpLegRef)
	if place || reason != "outside_price_limit" {
		t.Fatalf("intraday の帯外 TP を載せた: place=%v reason=%q", place, reason)
	}
}

// 基準値段が無い intraday は何も変えない(判定できないことを理由に形を変えない)。
func TestProtectiveTakeProfitOnBoard_IntradayWithoutReferenceKeepsTakeProfit(t *testing.T) {
	place, reason := ProtectiveTakeProfitOnBoard(order.HoldingIntraday, order.SideSell, tpLegOut, 0)
	if !place || reason != "reference_unavailable" {
		t.Fatalf("place=%v reason=%q, want true / reference_unavailable", place, reason)
	}
}

func TestProtectiveTakeProfitOnBoard_NoTakeProfitLeg(t *testing.T) {
	for _, h := range []order.HoldingMode{order.HoldingIntraday, order.HoldingMultiday} {
		if place, reason := ProtectiveTakeProfitOnBoard(h, order.SideSell, 0, tpLegRef); place || reason != "no_take_profit" {
			t.Fatalf("%s: TP=0 で place=%v reason=%q", h, place, reason)
		}
	}
}
