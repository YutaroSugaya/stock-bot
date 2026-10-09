-- 0007 down: advisor_run_id 参照列とインデックスを落とす。
-- バックフィル値も列ごと消える(up の再適用で復元可能 — advisor_runs は残る)。

DROP INDEX IF EXISTS strategy_configs_advisor_run_idx;

ALTER TABLE strategy_configs DROP COLUMN IF EXISTS advisor_run_id;
