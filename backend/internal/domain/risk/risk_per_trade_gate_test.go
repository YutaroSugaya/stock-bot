package risk

import (
	"strings"
	"testing"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/strategy"
)

// 1本あたりの想定損失(建値から SL までの距離 × 株数)に上限を置くゲート。
//
// 🛑 **これは SL を狭めるものではない。** SL の位置は戦略が決めたまま 1ミリも動かさず、
// 「その SL だと 1 本で上限を超える」候補を**建てない**だけ(母集団のフィルタであって
// 出口の幾何の変更ではない)。狭めると平均回帰が戻る前に振り落とされる本が増えるが、
// その当否を測る MFE/MAE がまだ無い。
//
// 🚨 **これは損失の上限ではない。** ギャップとストップ安は逆指値をすり抜けるので、
// 実損は上限を超えうる(6841 の例: 計画 32,500 に対し 4,200 で寄れば 52,700)。
// 上限が保証するのは「**建てる前に分かる計画損失**がここまで」であって最悪損失ではない。

func riskPerTradeSnap(cap int) AccountSnapshot {
	return AccountSnapshot{MaxRiskPerTradeJPY: cap}
}

func TestEvaluateStructural_RejectsRiskPerTradeOverCap(t *testing.T) {
	sig := passingSignal()
	sig.StopLossJPY = 450 // 450 × 100株 = 45,000
	sig.Quantity = 100

	d := EvaluateStructural(sig, passingConfig(), riskPerTradeSnap(35000), passingSummary())
	if d.Allowed {
		t.Fatal("想定損失 45,000 > 上限 35,000 なのに通した")
	}
	if !strings.Contains(d.Reason, "risk_per_trade 45000 > cap 35000") {
		t.Fatalf("理由に実額と上限の両方が要る(人間が config を直す材料になる): %q", d.Reason)
	}
}

// 🛑 上限ちょうどは通す。`>=` で切ると、上限に等しい候補が黙って落ちて
// 「35,000 まで許す」と読んだ人間の意図とズレる。
func TestEvaluateStructural_AllowsRiskPerTradeExactlyAtCap(t *testing.T) {
	sig := passingSignal()
	sig.StopLossJPY = 350 // 350 × 100株 = 35,000 ちょうど
	sig.Quantity = 100

	if d := EvaluateStructural(sig, passingConfig(), riskPerTradeSnap(35000), passingSummary()); !d.Allowed {
		t.Fatalf("上限ちょうどを落とした: %q", d.Reason)
	}
}

// 🛑 cap 0 = 無効。research(紙)と harvest は資本リスクがゼロなので、
// 全トリガー採用の標本をこの理由で censoring しない(max_gross_notional_ratio と同じ扱い)。
func TestEvaluateStructural_RiskPerTradeCapZeroDisablesGate(t *testing.T) {
	sig := passingSignal()
	sig.StopLossJPY = 100000 // 途方もない額でも cap 0 なら見ない
	sig.Quantity = 100

	if d := EvaluateStructural(sig, passingConfig(), riskPerTradeSnap(0), passingSummary()); !d.Allowed {
		t.Fatalf("cap 0(無効)なのに落とした: %q", d.Reason)
	}
}

// 🛑 SL の無いシグナルはこのゲートの担当ではない。落とすなら別の理由で落とす —
// ここで拾うと「守りが無い」と「1本が重すぎる」が同じ理由に混ざり、
// signal_rejections を読んでも原因が分からなくなる。
func TestEvaluateStructural_RiskPerTradeIgnoresSignalWithoutStop(t *testing.T) {
	sig := passingSignal()
	sig.StopLossJPY = 0
	sig.Quantity = 100

	if d := EvaluateStructural(sig, passingConfig(), riskPerTradeSnap(1), passingSummary()); !d.Allowed {
		t.Fatalf("SL の無いシグナルをこのゲートで落とした: %q", d.Reason)
	}
}

// 🚨 **構造ゲート(口座照会の前)で落ちること。** collateral 段に置くと、
// 建てないと分かっている候補のために立花へ wire 3 リクエストを払う
// (API 予算を焼いたのはまさにこの形)。
//
// EvaluateStructural は broker 由来のフィールドを 1 つも読まない契約なので、
// 未照会を表す MarginStatusUnknown を立てたままでも判定できることで示す。
func TestEvaluateStructural_RiskPerTradeRejectsBeforeAnyBrokerQuery(t *testing.T) {
	sig := passingSignal()
	sig.StopLossJPY = 450
	sig.Quantity = 100

	snap := riskPerTradeSnap(35000)
	snap.MarginStatusUnknown = true // まだ口座を 1 度も訊いていない状態

	d := EvaluateStructural(sig, passingConfig(), snap, passingSummary())
	if d.Allowed {
		t.Fatal("口座照会の前に落ちなかった")
	}
	if !strings.Contains(d.Reason, "risk_per_trade") {
		t.Fatalf("collateral 段の理由に化けている: %q", d.Reason)
	}
}

// entry 以外(決済・NO_TRADE)は素通し。守りを置く/閉じる方向の動きを止めない。
func TestEvaluateStructural_RiskPerTradeIgnoresNonEntry(t *testing.T) {
	sig := passingSignal()
	sig.Decision = strategy.DecisionNoTrade
	sig.StopLossJPY = 100000

	if d := EvaluateStructural(sig, passingConfig(), riskPerTradeSnap(1), &market.MarketSummary{}); !d.Allowed {
		t.Fatalf("非 entry を落とした: %q", d.Reason)
	}
}
