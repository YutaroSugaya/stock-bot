-- 逆操作: 列を落とす。失われるのは非正規化した写しだけで、正本(config_id →
-- strategy_configs.strategy_name)は残るので情報は消えない。
ALTER TABLE positions DROP COLUMN IF EXISTS strategy_name;
