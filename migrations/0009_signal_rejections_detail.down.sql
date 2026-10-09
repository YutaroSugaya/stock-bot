-- 0009 down: detail 列を落とす。

ALTER TABLE signal_rejections DROP COLUMN IF EXISTS detail;
