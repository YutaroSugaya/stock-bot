-- 逆操作: (symbol, mode) の一意 index に戻す。
--
-- 🛑 戻す前に**同一 (symbol, mode) の active が 2 つ以上ある状態を解消する**必要がある。
-- 新しい方(activated_at が最新、同着は config_id)だけを active に残し、残りは
-- 'expired' にする。行は消さない — strategy_configs は「その時どの config で動いていたか」の
-- 全文監査であり、positions.config_id の FK 参照先でもある。
UPDATE strategy_configs sc
SET status = 'expired'
WHERE sc.status = 'active'
  AND EXISTS (
    SELECT 1 FROM strategy_configs t
    WHERE t.status = 'active'
      AND t.symbol = sc.symbol
      AND t.mode = sc.mode
      AND (t.activated_at, t.config_id) > (sc.activated_at, sc.config_id)
  );

DROP INDEX IF EXISTS strategy_configs_active_live_uidx;
DROP INDEX IF EXISTS strategy_configs_active_uidx;

CREATE UNIQUE INDEX IF NOT EXISTS strategy_configs_active_uidx
    ON strategy_configs (symbol, mode) WHERE status = 'active';
