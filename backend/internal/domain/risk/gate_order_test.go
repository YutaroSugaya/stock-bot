package risk

import (
	"strings"
	"testing"
)

// The structural gate is an ordered table (item 4 of the structural refactor).
// The order is part of the contract: the never-overridable brakes run first so a
// tripped emergency is reported as "emergency_stop" and not as whatever softer
// cap happens to be hit too; the spread guard runs last because it is the only
// gate that reads the quote.
func TestStructuralGateOrderIsPinned(t *testing.T) {
	want := []string{
		"hard_brakes",
		"exec_kind_coherence",
		"tick_alignment",
		"risk_per_trade",
		"cooldown",
		"consecutive_losses",
		"position_count_caps",
		"account_entries_per_day",
		"symbol_budget",
		"nanpin_block",
		"intraday_once_per_day",
		"window_throttles",
		"direction",
		"spread",
	}
	got := structuralGateNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("gate order changed:\n got %v\nwant %v", got, want)
	}
}
