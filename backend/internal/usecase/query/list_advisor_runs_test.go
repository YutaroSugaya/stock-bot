package query

import (
	"context"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/port"
)

type fakeRunRepo struct {
	runs     []port.AdvisorRunRecord
	gotLimit int
	gotSym   string
	err      error
}

func (f *fakeRunRepo) Insert(context.Context, port.AdvisorRunRecord) error { return nil }

func (f *fakeRunRepo) List(_ context.Context, symbol string, limit int) ([]port.AdvisorRunRecord, error) {
	f.gotSym, f.gotLimit = symbol, limit
	return f.runs, f.err
}

func TestListAdvisorRunsMapsView(t *testing.T) {
	start := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	repo := &fakeRunRepo{runs: []port.AdvisorRunRecord{{
		RunID: "r1", Symbol: "7203", Status: port.AdvisorRunSuccess,
		StartedAt: start, FinishedAt: start.Add(12 * time.Second),
		RegimeType: "panic_selloff", RegimeConfidence: 0.7, RegimeReason: "5日で-12%",
		ParsedYAML: "name: bnf_reversion\n",
	}}}
	q := NewListAdvisorRuns(repo)

	out, err := q.Execute(context.Background(), "", 20)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("views = %d, want 1", len(out))
	}
	v := out[0]
	if v.RunID != "r1" || v.Symbol != "7203" || v.Status != "success" {
		t.Fatalf("view = %+v", v)
	}
	if v.DurationMS != 12000 {
		t.Fatalf("duration = %d, want 12000", v.DurationMS)
	}
	if v.RegimeType != "panic_selloff" || v.RegimeReason != "5日で-12%" || v.RegimeConfidence != 0.7 {
		t.Fatalf("regime not mapped: %+v", v)
	}
	if !strings.Contains(v.ParsedYAML, "bnf_reversion") {
		t.Fatalf("parsed yaml not mapped: %q", v.ParsedYAML)
	}
	if !v.Promotable {
		t.Fatal("success run should be Promotable (= config になり得た判断)")
	}
}

func TestListAdvisorRunsTruncatesYAML(t *testing.T) {
	repo := &fakeRunRepo{runs: []port.AdvisorRunRecord{{
		RunID: "r1", Status: port.AdvisorRunSuccess, ParsedYAML: strings.Repeat("x", 10000),
	}}}
	out, err := NewListAdvisorRuns(repo).Execute(context.Background(), "", 5)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out[0].ParsedYAML) > 4096 {
		t.Fatalf("yaml len = %d, want <= 4096", len(out[0].ParsedYAML))
	}
}

// fail-close の理由が見えないと運用できない。
func TestListAdvisorRunsKeepsFailures(t *testing.T) {
	repo := &fakeRunRepo{runs: []port.AdvisorRunRecord{
		{RunID: "r2", Status: port.AdvisorRunCLIError, ErrorMsg: "usage limit", UsageLimited: true},
	}}
	out, err := NewListAdvisorRuns(repo).Execute(context.Background(), "7203", 5)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out[0].Status != "cli_error" || out[0].ErrorMsg != "usage limit" || !out[0].UsageLimited {
		t.Fatalf("failure not surfaced: %+v", out[0])
	}
	if out[0].Promotable {
		t.Fatal("cli_error must not be Promotable")
	}
	if repo.gotSym != "7203" || repo.gotLimit != 5 {
		t.Fatalf("repo args = %q/%d", repo.gotSym, repo.gotLimit)
	}
}

// UI の取り違えで DB を舐めない。
func TestListAdvisorRunsClampsLimit(t *testing.T) {
	repo := &fakeRunRepo{}
	q := NewListAdvisorRuns(repo)
	if _, err := q.Execute(context.Background(), "", 0); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if repo.gotLimit != 20 {
		t.Fatalf("limit(0) = %d, want default 20", repo.gotLimit)
	}
	if _, err := q.Execute(context.Background(), "", 5000); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if repo.gotLimit != 200 {
		t.Fatalf("limit(5000) = %d, want clamp 200", repo.gotLimit)
	}
}

func TestListAdvisorRunsNilRepo(t *testing.T) {
	out, err := NewListAdvisorRuns(nil).Execute(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("nil repo must not error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("views = %d, want 0", len(out))
	}
}
