-- 0007: strategy_configs.advisor_run_id — trades → positions → strategy_configs
-- → advisor_runs を本物の参照チェーンにする(これまで config_id(JST+銘柄)と
-- run_id(UTC+乱数)の時刻突き合わせでしか繋がらず、秒厳密一致では 79.2% しか
-- 当たらなかった)。
--
-- FK は貼らない: 人手・テスト・sentinel(external / default no_trade)で作った
-- config には対応する run が無い。NULL 許容の参照列 + 部分インデックスのみ。

ALTER TABLE strategy_configs ADD COLUMN IF NOT EXISTS advisor_run_id TEXT;

CREATE INDEX IF NOT EXISTS strategy_configs_advisor_run_idx
    ON strategy_configs (advisor_run_id) WHERE advisor_run_id IS NOT NULL;

-- 既存 run のバックフィル(過去データを捨てない)。ただし**曖昧なら埋めない**:
-- 同一銘柄の 60 秒窓に run が 2 件以上ある config は NULL のまま残す(推測で
-- 埋めた誤リンクは、リンク無しより悪い)。埋まらなかった件数の確認クエリは
-- docs/workflows/MIGRATIONS.md の 0007 の項。
--
-- config_id の時刻形式(YYYYMMDD-HHMMSS-<symbol>・JST)に一致する行だけを対象に
-- する — sentinel(`external`)や default config は形式が違い、to_timestamp が
-- エラーになるため、CTE の WHERE で先に絞ってから SELECT 句でパースする。
-- advisor_run_id IS NULL 条件により再適用しても上書きしない(idempotent)。
WITH cand AS (
    SELECT sc.config_id,
           sc.symbol,
           (to_timestamp(left(sc.config_id, 15), 'YYYYMMDD-HH24MISS')::timestamp
              AT TIME ZONE 'Asia/Tokyo') AS armed_at
    FROM strategy_configs sc
    WHERE sc.advisor_run_id IS NULL
      AND sc.config_id ~ '^[0-9]{8}-[0-9]{6}-'
), matched AS (
    -- success 以外(cli_error / timeout 等)は config を書いた run ではあり得ない
    -- (promote は success run のみ)ので候補から外す。これが無いと「窓内が失敗
    -- run 1件だけ」の config を誤リンクし、それが本物の FK として復元経路に乗る。
    SELECT c.config_id,
           min(a.run_id) AS run_id,
           count(*)      AS n
    FROM cand c
    JOIN advisor_runs a
      ON a.symbol = c.symbol
     AND a.status = 'success'
     AND abs(extract(epoch FROM (a.started_at - c.armed_at))) <= 60
    GROUP BY c.config_id
)
UPDATE strategy_configs sc
SET advisor_run_id = m.run_id
FROM matched m
WHERE sc.config_id = m.config_id
  AND m.n = 1;
