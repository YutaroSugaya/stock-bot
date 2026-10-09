-- 0003_position_extension_early_exit — persist the extension / early-exit exit
-- geometry frozen onto a Position at entry.
--
-- Why: ExecuteOrder already passes these four values into the repo and the
-- in-memory repo stores them, but the Postgres schema had no columns — so under
-- STOCKBOT_DATABASE_URL they read back as 0 and position.EvaluateExit's
-- extension / early-exit branches were silently DEAD. The backtest engine does
-- honour them, so a strategy validated in backtest exited differently when run
-- on Postgres, with no error anywhere. That divergence corrupts the very forward
-- record the edge is judged on (CLAUDE.md: config 凍結 / エッジ規律).
--
-- up is idempotent (ADD COLUMN IF NOT EXISTS). Defaults are 0 = "feature off",
-- which is exactly how existing rows behaved, so the backfill is a no-op.

ALTER TABLE positions
    ADD COLUMN IF NOT EXISTS extension_max_minutes          INTEGER          NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS extension_unrealized_threshold DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS early_exit_window_minutes      INTEGER          NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS early_exit_target_ticks        DOUBLE PRECISION NOT NULL DEFAULT 0;
