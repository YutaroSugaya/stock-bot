-- 0001_init down — inverse of the up migration (down must be the real
-- inverse, not comment-only). Drop in reverse dependency order.

DROP TABLE IF EXISTS signal_rejections;
DROP TABLE IF EXISTS candles;
DROP TABLE IF EXISTS trades;
DROP TABLE IF EXISTS positions_live;
DROP INDEX IF EXISTS positions_open_idx;
DROP TABLE IF EXISTS positions;
DROP INDEX IF EXISTS strategy_configs_active_uidx;
DROP TABLE IF EXISTS strategy_configs;
