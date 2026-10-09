-- 0004_advisor_runs_model — record WHICH LLM generation produced each advisor run.
--
-- Why: advisor は `claude -p` のサブプロセスで走るが、素のテキスト出力はモデル名を
-- 報告しない。そのため「この config はどの世代が書いたか」が run 単位では残らず、
-- 世代交代を跨いだ forward 標本を
-- 後から切り分けられなかった。日付での突き合わせは対応表の手動メンテに依存し、
-- 記録漏れがそのまま provenance の欠落になる。
--
-- `--output-format json` の modelUsage から読み取った値を入れる。空文字 = 不明
-- (古い CLI / envelope が返らなかった run)。既存行は '' のままで、それは
-- 「不明」という正しい状態なので backfill しない。
--
-- 観測専用: 取引経路はこの列を読まない。up は idempotent。

ALTER TABLE advisor_runs
    ADD COLUMN IF NOT EXISTS model TEXT NOT NULL DEFAULT '';
