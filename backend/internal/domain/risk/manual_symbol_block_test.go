package risk

import (
	"testing"

	"stockbot/backend/internal/domain/strategy"
)

// live の銘柄ごとの新規停止(人間のボタン)。どの経路の entry も越えられない。
func TestManualSymbolBlock_RejectsEntry(t *testing.T) {
	snap := AccountSnapshot{ManualSymbolBlocked: true}
	for name, eval := range map[string]func() Decision{
		"structural": func() Decision { return EvaluateStructural(passingSignal(), passingConfig(), snap, passingSummary()) },
		"signal":     func() Decision { return EvaluateSignal(passingSignal(), passingConfig(), snap, passingSummary()) },
		"hard":       func() Decision { return EvaluateHardSafety(passingSignal(), passingConfig(), snap, passingSummary()) },
	} {
		if d := eval(); d.Allowed || d.Reason != ReasonManualSymbolBlock {
			t.Errorf("%s: got allowed=%v reason=%q, want %q", name, d.Allowed, d.Reason, ReasonManualSymbolBlock)
		}
	}
}

// 停止のファイルが読めない・壊れているときは、その track の新規を全部止める(fail-close)。
func TestSymbolBlocksUnreadable_RejectsEveryEntry(t *testing.T) {
	snap := AccountSnapshot{SymbolBlocksUnreadable: true}
	for name, eval := range map[string]func() Decision{
		"structural": func() Decision { return EvaluateStructural(passingSignal(), passingConfig(), snap, passingSummary()) },
		"hard":       func() Decision { return EvaluateHardSafety(passingSignal(), passingConfig(), snap, passingSummary()) },
	} {
		if d := eval(); d.Allowed || d.Reason != ReasonSymbolBlocksUnreadable {
			t.Errorf("%s: got allowed=%v reason=%q, want %q", name, d.Allowed, d.Reason, ReasonSymbolBlocksUnreadable)
		}
	}
}

// 止めるのは新規だけ。決済などの非 entry は素通しする。
func TestManualSymbolBlock_DoesNotTouchNonEntries(t *testing.T) {
	snap := AccountSnapshot{ManualSymbolBlocked: true, SymbolBlocksUnreadable: true}
	sig := strategy.Signal{Decision: strategy.DecisionNoTrade}
	if d := EvaluateHardSafety(sig, passingConfig(), snap, passingSummary()); !d.Allowed {
		t.Fatalf("非 entry を止めた: %q", d.Reason)
	}
}
