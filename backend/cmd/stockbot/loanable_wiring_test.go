package main

import (
	"strings"
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/strategy"
)

// 🚨 **貸借ゲートの production 配線を縛る**。監査で
// 「`Configured` をメソッド値だけで配線していたので常に true になり、文書が約束する
// `loanable_list_missing` が production では一度も記録されない」という配線バグが
// 出ている。配線はソースを読まないと分からないので pin する。
func TestLoanableGateIsWiredWithConfiguredFromTheListItself(t *testing.T) {
	src := mustReadSource(t, "symbol_bundle_wiring.go")
	// Configured は**一覧の有無**から立てる(メソッド値だけだと常に true)。
	if !strings.Contains(src, "Configured: len(d.hardLimits.LoanableSymbols) > 0") {
		t.Error("cycle.Loanable.Configured が一覧の有無から立っていない — " +
			"loanable_list_missing が production で一度も記録されなくなる")
	}
	if !strings.Contains(src, "Allows:     d.hardLimits.AllowsShortSymbol") {
		t.Error("cycle.Loanable.Allows が配線されていない")
	}
	// 枠の間引き側は **一覧が空なら nil**(= 間引かない)。ここを常に渡すと発注前
	// ゲートまで届かず、診断行が消える。
	main := mustReadSource(t, "main.go")
	if !strings.Contains(main, "ShortAllowed: app.ShortAllowedOrNil(hl)") {
		t.Error("ShortAllowed が ShortAllowedOrNil 経由でない — 一覧未 commit のときに " +
			"loanable_list_missing が signal_rejections に残らなくなる")
	}
}

// ゲートの述語そのもの(fail-close の 2 状態)。
func TestLoanableGateDistinguishesMissingListFromNotLoanable(t *testing.T) {
	sell := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideSell, Symbol: "7203", Quantity: 100}

	// 一覧が未 commit。
	empty := &config.HardLimits{AllowedSymbols: []string{"7203"}}
	d := risk.EvaluateShortLoanable(sell, risk.LoanableSymbols{
		Configured: len(empty.LoanableSymbols) > 0, Allows: empty.AllowsShortSymbol})
	if d.Allowed || d.Reason != "loanable_list_missing" {
		t.Fatalf("一覧未 commit の理由 = %q(allowed=%v), want loanable_list_missing", d.Reason, d.Allowed)
	}

	// 一覧はあるが載っていない。
	withList := &config.HardLimits{AllowedSymbols: []string{"7203", "6501"}, LoanableSymbols: []string{"6501"}}
	d = risk.EvaluateShortLoanable(sell, risk.LoanableSymbols{
		Configured: len(withList.LoanableSymbols) > 0, Allows: withList.AllowsShortSymbol})
	if d.Allowed || !strings.HasPrefix(d.Reason, "not_loanable") {
		t.Fatalf("非貸借の理由 = %q(allowed=%v), want not_loanable…", d.Reason, d.Allowed)
	}

	// 🛑 買いには一切効かない。
	buy := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "7203", Quantity: 100}
	if d := risk.EvaluateShortLoanable(buy, risk.LoanableSymbols{}); !d.Allowed {
		t.Fatalf("一覧が空なのに買いを落とした: %q", d.Reason)
	}
}
