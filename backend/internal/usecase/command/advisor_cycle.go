package command

import (
	"context"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// InfoLogger is a local interface so usecase never imports slog (層規約).
// *slog.Logger satisfies it as-is.
type InfoLogger interface {
	Info(msg string, kv ...any)
}

// AdvisorCycle runs ONE advisor invocation and validates its output into an
// arm-ready config. The LLM is never on the order path: it only produces a config
// the Promoter validates and the deterministic engine executes.
type AdvisorCycle struct {
	Advisor      port.Advisor
	Promoter     *Promoter
	BuildSummary func() *market.MarketSummary
	Notifier     port.Notifier             // optional
	Runs         port.AdvisorRunRepository // optional; audit trail of every run
	Logger       InfoLogger                // optional; *slog.Logger fits
}

// record is best-effort: a failed audit write is logged and swallowed —
// observability must never block or change a trading decision.
func (u *AdvisorCycle) record(ctx context.Context, run *port.AdvisorRun, symbol string, cfg *config.StrategyConfig) {
	if u.Runs == nil || run == nil {
		return
	}
	rec := port.AdvisorRunRecord{
		RunID: run.RunID, Symbol: symbol, Status: run.Status, UsageLimited: run.UsageLimited,
		StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
		InputJSON: string(run.InputJSON), OutputYAML: string(run.OutputYAML),
		ParsedYAML: string(run.ParsedYAML), ErrorMsg: run.ErrorMsg,
		Model: run.Model,
	}
	if cfg != nil {
		rec.RegimeType = cfg.MarketRegime.Type
		rec.RegimeConfidence = cfg.MarketRegime.Confidence
		rec.RegimeReason = cfg.MarketRegime.Reason
	}
	if err := u.Runs.Insert(ctx, rec); err != nil {
		u.log("advisor_run_persist_failed", "run_id", run.RunID, "err", err.Error())
	}
}

// Run returns cfg=nil to mean "arm nothing" (a no_trade config comes back
// non-nil). A non-success run or a rejected config is NOT an err — it is
// (nil, run, nil), fail-closed; err is reserved for genuine infra failures.
func (u *AdvisorCycle) Run(ctx context.Context) (*config.StrategyConfig, *port.AdvisorRun, error) {
	summary := u.BuildSummary()
	run, err := u.Advisor.Generate(ctx, summary)
	if err != nil {
		return nil, nil, err
	}
	if run == nil { // contract violation by an Advisor impl — fail closed, never panic
		u.log("advisor_nil_run")
		return nil, nil, nil
	}
	symbol := ""
	if summary != nil {
		symbol = summary.Symbol
	}
	if !run.Status.Promotable() {
		u.log("advisor_run_not_promotable", "status", string(run.Status), "err", run.ErrorMsg, "usage_limited", run.UsageLimited)
		u.notify(ctx, "warn", "advisor_run_failed",
			"advisor run status="+string(run.Status)+" err="+run.ErrorMsg)
		u.record(ctx, run, symbol, nil)
		return nil, run, nil
	}
	cfg, perr := u.Promoter.Promote(run.ParsedYAML)
	if perr != nil {
		u.log("advisor_promote_rejected", "run_id", run.RunID, "err", perr.Error())
		u.notify(ctx, "warn", "advisor_promote_rejected", perr.Error())
		u.record(ctx, run, symbol, nil)
		return nil, run, nil
	}
	// Provenance が YAML から書けない(`yaml:"-"`)ので、ここが唯一の書き手。
	cfg.AdvisorRunID = run.RunID
	// LLM の判断理由をログに残す — 後で forward 記録を読み直すときの一次資料。
	u.log("advisor_promote_ok", "run_id", run.RunID, "symbol", cfg.Symbol,
		"strategy", string(cfg.StrategyName), "config_id", cfg.ConfigID,
		"regime", cfg.MarketRegime.Type, "confidence", cfg.MarketRegime.Confidence,
		"reason", cfg.MarketRegime.Reason)
	u.record(ctx, run, symbol, cfg)
	return cfg, run, nil
}

func (u *AdvisorCycle) log(msg string, kv ...any) {
	if u.Logger != nil {
		u.Logger.Info(msg, kv...)
	}
}

func (u *AdvisorCycle) notify(ctx context.Context, level, event, body string) {
	if u.Notifier != nil {
		_ = u.Notifier.Notify(ctx, level, event, body)
	}
}
