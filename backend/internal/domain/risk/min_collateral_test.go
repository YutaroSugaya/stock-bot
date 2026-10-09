package risk

import (
	"strings"
	"testing"
)

// 🚨 **最低委託保証金(hard_limits `margin.min_collateral_jpy`)を割ったら新規 entry を断る**。
// それまでこの値は yaml と「値が 30 万以上か」のテストにしか無く、
// 本番コードからの参照が 0 件 = 効いている安全装置に見えて何もしていなかった。
func TestEvaluateCollateral_RejectsBelowMinimumCollateral(t *testing.T) {
	snap := AccountSnapshot{
		CollateralRequiredJPY: 75000,
		AvailableToTradeJPY:   1_000_000,
		CollateralJPY:         299_999,
		MinCollateralJPY:      300_000,
	}
	d := EvaluateCollateral(phaseSig(), snap)
	if d.Allowed || !strings.HasPrefix(d.Reason, ReasonCollateralBelowMinimum) {
		t.Fatalf("保証金 %d < 下限 %d を通した / 理由が違う: %+v", snap.CollateralJPY, snap.MinCollateralJPY, d)
	}

	snap.CollateralJPY = 300_000 // ちょうど下限は建てられる
	if d := EvaluateCollateral(phaseSig(), snap); !d.Allowed {
		t.Fatalf("下限ちょうどで reject: %q", d.Reason)
	}
}

// 0 = 無効(backtest / 既存の配線の挙動を変えない)。
func TestEvaluateCollateral_ZeroMinimumDisables(t *testing.T) {
	snap := AccountSnapshot{AvailableToTradeJPY: 1_000_000, CollateralJPY: 0}
	if d := EvaluateCollateral(phaseSig(), snap); !d.Allowed {
		t.Fatalf("下限 0 なのに reject: %q", d.Reason)
	}
}

// 未照会(= 保証金が分からない)は下限の判定より先に fail-close する。
func TestEvaluateCollateral_UnknownMarginFailsCloseBeforeMinimum(t *testing.T) {
	snap := AccountSnapshot{MarginStatusUnknown: true, MinCollateralJPY: 300_000, CollateralJPY: 1_000_000}
	if d := EvaluateCollateral(phaseSig(), snap); d.Allowed || d.Reason != "margin_status_unavailable" {
		t.Fatalf("未照会で fail-close しない: %+v", d)
	}
}
