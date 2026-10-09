-- 0010 down: MAE の列を落とす。記録専用の列で、どの決済規則も読んでいないので
-- 落としても挙動は変わらない(失われるのは反実仮想の材料だけ)。

ALTER TABLE positions DROP COLUMN IF EXISTS trough_unrealized_jpy;
