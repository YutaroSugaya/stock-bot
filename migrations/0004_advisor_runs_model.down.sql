-- 0004_advisor_runs_model (down) — exact inverse: drop the column. 戻すと
-- run 単位の provenance が失われ、世代の切り分けは再び日付突き合わせ
-- だけになる。列を書かなくなったコードと同時に戻すこと。

ALTER TABLE advisor_runs
    DROP COLUMN IF EXISTS model;
