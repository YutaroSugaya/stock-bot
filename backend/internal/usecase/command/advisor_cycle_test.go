package command

import (
	"context"
	"testing"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

type stubAdvisor struct {
	run *port.AdvisorRun
	err error
}

func (s stubAdvisor) Generate(ctx context.Context, _ *market.MarketSummary) (*port.AdvisorRun, error) {
	return s.run, s.err
}

func cycle(run *port.AdvisorRun) *AdvisorCycle {
	return &AdvisorCycle{
		Advisor:      stubAdvisor{run: run},
		Promoter:     &Promoter{HardLimits: testHardLimits(), ExpectedSymbol: "7203"},
		BuildSummary: func() *market.MarketSummary { return &market.MarketSummary{Symbol: "7203"} },
	}
}

type recRuns struct{ rows []port.AdvisorRunRecord }

func (r *recRuns) Insert(_ context.Context, rec port.AdvisorRunRecord) error {
	r.rows = append(r.rows, rec)
	return nil
}
func (r *recRuns) List(_ context.Context, _ string, _ int) ([]port.AdvisorRunRecord, error) {
	return r.rows, nil
}

func TestCycle_PersistsAuditTrailWithReason(t *testing.T) {
	y := bnfPaperYAML + "market_regime:\n  type: panic_crash\n  confidence: 0.8\n  reason: 25日線-15%かつ出来高3倍\n"
	runs := &recRuns{}
	c := cycle(&port.AdvisorRun{RunID: "r1", Status: port.AdvisorRunSuccess, ParsedYAML: []byte(y)})
	c.Runs = runs
	cfg, _, err := c.Run(context.Background())
	if err != nil || cfg == nil {
		t.Fatalf("expected promote, got %v %v", cfg, err)
	}
	if len(runs.rows) != 1 {
		t.Fatalf("want 1 audit row, got %d", len(runs.rows))
	}
	got := runs.rows[0]
	if got.RunID != "r1" || got.Symbol != "7203" || got.Status != port.AdvisorRunSuccess {
		t.Fatalf("bad audit row: %+v", got)
	}
	if got.RegimeType != "panic_crash" || got.RegimeReason == "" || got.RegimeConfidence != 0.8 {
		t.Fatalf("rationale not recorded: %+v", got)
	}
}

// 世代を監査行に残さないと、世代交代を跨いだ forward 標本を後から切り分けられない。
func TestCycle_PersistsModel(t *testing.T) {
	runs := &recRuns{}
	c := cycle(&port.AdvisorRun{
		RunID: "r3", Status: port.AdvisorRunSuccess,
		ParsedYAML: []byte(bnfPaperYAML), Model: "claude-opus-5",
	})
	c.Runs = runs
	if _, _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runs.rows) != 1 || runs.rows[0].Model != "claude-opus-5" {
		t.Fatalf("model が監査行に落ちていない: %+v", runs.rows)
	}
}

func TestCycle_PersistsFailedRun(t *testing.T) {
	runs := &recRuns{}
	c := cycle(&port.AdvisorRun{RunID: "r2", Status: port.AdvisorRunCLIError, ErrorMsg: "boom"})
	c.Runs = runs
	if _, _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runs.rows) != 1 || runs.rows[0].Status != port.AdvisorRunCLIError {
		t.Fatalf("failed run must still be audited, got %+v", runs.rows)
	}
}

// run_id が strategy_configs.advisor_run_id へ流れ、trades → positions →
// strategy_configs → advisor_runs が時刻突き合わせなしの参照チェーンになる。
func TestCycle_StampsAdvisorRunIDOnConfig(t *testing.T) {
	run := &port.AdvisorRun{RunID: "20260731T010203Z-abcd1234", Status: port.AdvisorRunSuccess, ParsedYAML: []byte(bnfPaperYAML)}
	cfg, _, err := cycle(run).Run(context.Background())
	if err != nil || cfg == nil {
		t.Fatalf("expected promote, got cfg=%v err=%v", cfg, err)
	}
	if cfg.AdvisorRunID != run.RunID {
		t.Fatalf("AdvisorRunID = %q, want %q", cfg.AdvisorRunID, run.RunID)
	}
}

// advisor_run_id は runtime が刻む出自メタデータで、LLM が書く項目ではない。
func TestPromote_AdvisorRunIDNotParseableFromYAML(t *testing.T) {
	y := bnfPaperYAML + "advisor_run_id: forged-run-id\n"
	cfg, err := (&Promoter{HardLimits: testHardLimits(), ExpectedSymbol: "7203"}).Promote([]byte(y))
	if err != nil {
		t.Fatalf("unknown yaml key は無害に無視されるべき: %v", err)
	}
	if cfg.AdvisorRunID != "" {
		t.Fatalf("YAML から advisor_run_id が注入された: %q", cfg.AdvisorRunID)
	}
}

func TestCycle_SuccessPromotes(t *testing.T) {
	run := &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(bnfPaperYAML)}
	cfg, gotRun, err := cycle(run).Run(context.Background())
	if err != nil || cfg == nil || gotRun != run {
		t.Fatalf("want armed cfg, got cfg=%v err=%v", cfg, err)
	}
}

func TestCycle_NonPromotableRunArmsNothing(t *testing.T) {
	run := &port.AdvisorRun{Status: port.AdvisorRunCLIError, ParsedYAML: []byte(bnfPaperYAML)}
	cfg, _, err := cycle(run).Run(context.Background())
	if err != nil || cfg != nil {
		t.Fatalf("cli_error must arm nothing: cfg=%v err=%v", cfg, err)
	}
}

// A rejected config arms nothing but is NOT an infra error.
func TestCycle_RejectedConfigArmsNothing(t *testing.T) {
	bad := []byte("config_id: x\nsymbol: \"7203\"\nstrategy_name: time_series_momentum\nmode: paper_config\nrisk:\n  quantity: 100\n")
	run := &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: bad}
	cfg, _, err := cycle(run).Run(context.Background())
	if err != nil || cfg != nil {
		t.Fatalf("rejected config must arm nothing: cfg=%v err=%v", cfg, err)
	}
}
