-- 0002_advisor_runs — audit trail for the LLM advisor (config-generation loop).
-- Every invocation is recorded, success or failure, so "その時 LLM が何を見て / 何を
-- 判断したか" survives restarts and can be reviewed against the forward record.
-- The advisor NEVER places orders; this table is observability only (no FK from
-- positions/trades) and nothing in the trading path reads it. up is idempotent.

CREATE TABLE IF NOT EXISTS advisor_runs (
    run_id            TEXT PRIMARY KEY,
    symbol            TEXT NOT NULL,
    -- success | cli_error | parse_error | timeout | empty_output
    -- (mirrors port.AdvisorRunStatus; only 'success' is promotable — extend the
    --  CHECK and the port string together, never one side alone)
    status            TEXT NOT NULL CHECK (status IN
                        ('success','cli_error','parse_error','timeout','empty_output')),
    usage_limited     BOOLEAN NOT NULL DEFAULT false,
    started_at        TIMESTAMPTZ NOT NULL,
    finished_at       TIMESTAMPTZ,
    input_json        TEXT NOT NULL DEFAULT '',  -- the MarketSummary the LLM saw
    output_yaml       TEXT NOT NULL DEFAULT '',  -- raw stdout (audit)
    parsed_yaml       TEXT NOT NULL DEFAULT '',  -- fence-stripped config (empty unless success)
    error_msg         TEXT NOT NULL DEFAULT '',
    -- the LLM's stated rationale (market_regime block); empty for non-success runs
    regime_type       TEXT NOT NULL DEFAULT '',
    regime_confidence DOUBLE PRECISION NOT NULL DEFAULT 0,
    regime_reason     TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- "直近の判断を新しい順に" と "この銘柄の判断履歴" が主クエリ。
CREATE INDEX IF NOT EXISTS advisor_runs_started_idx ON advisor_runs (started_at DESC);
CREATE INDEX IF NOT EXISTS advisor_runs_symbol_started_idx ON advisor_runs (symbol, started_at DESC);
