package port

import (
	"context"
	"time"

	"stockbot/backend/internal/domain/market"
)

// AdvisorRunStatus is the terminal outcome of one advisor (LLM) invocation.
// These string values are persisted (advisor_runs.status) — do not rename.
type AdvisorRunStatus string

const (
	AdvisorRunSuccess    AdvisorRunStatus = "success"
	AdvisorRunCLIError   AdvisorRunStatus = "cli_error"
	AdvisorRunParseError AdvisorRunStatus = "parse_error"
	AdvisorRunTimeout    AdvisorRunStatus = "timeout"
	AdvisorRunEmpty      AdvisorRunStatus = "empty_output"
)

// Promotable is fail-closed: success only. Any other status (including a future
// unknown one) must skip promotion so a broken advisor run can never arm a trade.
func (s AdvisorRunStatus) Promotable() bool { return s == AdvisorRunSuccess }

type AdvisorRun struct {
	RunID      string
	StartedAt  time.Time
	FinishedAt time.Time
	InputJSON  []byte
	OutputYAML []byte // raw stdout, kept verbatim for audit
	ParsedYAML []byte // fence-stripped, ready for yaml.Unmarshal
	Status     AdvisorRunStatus
	ErrorMsg   string

	// Model is read back from the CLI's own report, not from config, so the run
	// records which LLM generation actually wrote the config. Empty = unknown
	// (CLI が envelope を返さなかった等)。provenance 専用 — 判定には使わない。
	Model string

	// UsageLimited marks a Claude usage/session-limit failure, not a transient
	// infra blip: the scheduler skips its fast-retry, which would only burn the
	// already-exhausted quota. Not persisted (Status stays cli_error).
	UsageLimited bool
}

// Advisor generates a candidate StrategyConfig (YAML) by invoking the LLM. It is
// NOT on the real-time order path and never places an order — the deterministic
// Promoter validates its output against hard_limits + the candidate menu.
// enabled=false / name=no_trade is the go/no-go "no". Default OFF.
type Advisor interface {
	Generate(ctx context.Context, summary *market.MarketSummary) (*AdvisorRun, error)
}

// AdvisorRunRecord is one persisted advisor invocation. Observability only: the
// trading path never reads it, and a write failure must never block trading.
type AdvisorRunRecord struct {
	RunID        string
	Symbol       string
	Status       AdvisorRunStatus
	UsageLimited bool
	StartedAt    time.Time
	FinishedAt   time.Time
	InputJSON    string
	OutputYAML   string
	ParsedYAML   string
	ErrorMsg     string
	Model        string
	// The LLM's stated rationale (market_regime); empty for non-success runs.
	RegimeType       string
	RegimeConfidence float64
	RegimeReason     string
}

type AdvisorRunRepository interface {
	Insert(ctx context.Context, r AdvisorRunRecord) error
	List(ctx context.Context, symbol string, limit int) ([]AdvisorRunRecord, error)
}
