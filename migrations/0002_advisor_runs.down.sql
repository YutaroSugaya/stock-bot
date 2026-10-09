-- 0002_advisor_runs (down) — exact inverse of the up: drop the indexes then the
-- table. advisor_runs is observability only, so no other table references it.

DROP INDEX IF EXISTS advisor_runs_symbol_started_idx;
DROP INDEX IF EXISTS advisor_runs_started_idx;
DROP TABLE IF EXISTS advisor_runs;
