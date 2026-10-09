package port_test

import (
	"context"
	"testing"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

type fakeAdvisor struct{ status port.AdvisorRunStatus }

func (f fakeAdvisor) Generate(ctx context.Context, s *market.MarketSummary) (*port.AdvisorRun, error) {
	return &port.AdvisorRun{Status: f.status}, nil
}

var _ port.Advisor = fakeAdvisor{}

// Pinned because these strings are persisted in advisor_runs.status — a rename
// would silently orphan the existing audit rows.
func TestAdvisorRun_StatusConstants(t *testing.T) {
	cases := map[port.AdvisorRunStatus]string{
		port.AdvisorRunSuccess:    "success",
		port.AdvisorRunCLIError:   "cli_error",
		port.AdvisorRunParseError: "parse_error",
		port.AdvisorRunTimeout:    "timeout",
		port.AdvisorRunEmpty:      "empty_output",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("status %q != %q", string(got), want)
		}
	}
}

func TestAdvisorRun_SuccessIsTheOnlyPromotableStatus(t *testing.T) {
	if !port.AdvisorRunSuccess.Promotable() {
		t.Fatal("success must be promotable")
	}
	for _, s := range []port.AdvisorRunStatus{
		port.AdvisorRunCLIError, port.AdvisorRunParseError,
		port.AdvisorRunTimeout, port.AdvisorRunEmpty,
	} {
		if s.Promotable() {
			t.Errorf("%q must NOT be promotable (fail-closed)", s)
		}
	}
}

// Guards against someone later making a failure status "promotable enough".
func TestAdvisorRun_OnlySuccessGatesPromotion(t *testing.T) {
	for _, tc := range []struct {
		status port.AdvisorRunStatus
		want   bool
	}{
		{port.AdvisorRunSuccess, true},
		{port.AdvisorRunCLIError, false},
		{port.AdvisorRunParseError, false},
		{port.AdvisorRunTimeout, false},
		{port.AdvisorRunEmpty, false},
		{port.AdvisorRunStatus("something_new"), false}, // unknown must fail closed
	} {
		run, err := fakeAdvisor{status: tc.status}.Generate(context.Background(), &market.MarketSummary{Symbol: "7203"})
		if err != nil {
			t.Fatal(err)
		}
		if got := run.Status.Promotable(); got != tc.want {
			t.Errorf("status %q Promotable()=%v want %v", tc.status, got, tc.want)
		}
	}
}
