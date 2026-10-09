package query

import (
	"context"

	"stockbot/backend/internal/port"
)

// AdvisorRunView は観測専用 — 取引経路はこれを一切読まない(LLM は発注経路に
// 入れない)。
type AdvisorRunView struct {
	RunID      string `json:"run_id"`
	Symbol     string `json:"symbol"`
	Status     string `json:"status"` // success | cli_error | parse_error | timeout | empty_output
	Promotable bool   `json:"promotable"`
	StartedAt  string `json:"started_at"`  // RFC3339 (空 = 未設定)
	FinishedAt string `json:"finished_at"` // RFC3339 (空 = 未設定)
	DurationMS int64  `json:"duration_ms"`
	// 非 success では空。
	RegimeType       string  `json:"regime_type"`
	RegimeConfidence float64 `json:"regime_confidence"`
	RegimeReason     string  `json:"regime_reason"`
	// UI 表示用に切り詰めてある(完全な監査は DB 側)。
	ParsedYAML   string `json:"parsed_yaml"`
	ErrorMsg     string `json:"error_msg"`
	UsageLimited bool   `json:"usage_limited"`
	// 生成した LLM 世代。空 = 不明(migration 0004 以前の行 / 古い CLI)。
	Model string `json:"model"`
}

type ListAdvisorRuns struct {
	runs port.AdvisorRunRepository
}

// NewListAdvisorRuns: a nil repository (advisor OFF) is valid and yields an
// empty list, so the panel shows 「判断なし」 instead of failing the dashboard.
func NewListAdvisorRuns(runs port.AdvisorRunRepository) *ListAdvisorRuns {
	return &ListAdvisorRuns{runs: runs}
}

const (
	advisorRunsDefaultLimit = 20
	advisorRunsMaxLimit     = 200
	advisorYAMLMaxBytes     = 4096
)

// Execute clamps limit so a UI mistake cannot sweep the whole table.
func (q *ListAdvisorRuns) Execute(ctx context.Context, symbol string, limit int) ([]AdvisorRunView, error) {
	if q == nil || q.runs == nil {
		return nil, nil
	}
	switch {
	case limit <= 0:
		limit = advisorRunsDefaultLimit
	case limit > advisorRunsMaxLimit:
		limit = advisorRunsMaxLimit
	}
	recs, err := q.runs.List(ctx, symbol, limit)
	if err != nil {
		return nil, err
	}
	out := make([]AdvisorRunView, 0, len(recs))
	for _, r := range recs {
		out = append(out, toAdvisorRunView(r))
	}
	return out, nil
}

func toAdvisorRunView(r port.AdvisorRunRecord) AdvisorRunView {
	v := AdvisorRunView{
		RunID: r.RunID, Symbol: r.Symbol, Status: string(r.Status),
		Promotable:       r.Status.Promotable(),
		RegimeType:       r.RegimeType,
		RegimeConfidence: r.RegimeConfidence,
		RegimeReason:     r.RegimeReason,
		ParsedYAML:       truncate(r.ParsedYAML, advisorYAMLMaxBytes),
		ErrorMsg:         r.ErrorMsg,
		UsageLimited:     r.UsageLimited,
		Model:            r.Model,
	}
	if !r.StartedAt.IsZero() {
		v.StartedAt = r.StartedAt.Format(rfc3339)
	}
	if !r.FinishedAt.IsZero() {
		v.FinishedAt = r.FinishedAt.Format(rfc3339)
		if !r.StartedAt.IsZero() {
			v.DurationMS = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
		}
	}
	return v
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"

// truncate caps at max bytes INCLUDING the marker, so the view never exceeds
// the budget.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const marker = "\n… (truncated)"
	if max <= len(marker) {
		return s[:max]
	}
	return s[:max-len(marker)] + marker
}
