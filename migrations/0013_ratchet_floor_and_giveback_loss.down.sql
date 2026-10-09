-- 逆操作: 床フラグの列を落とし、'ratchet_giveback_loss' を CHECK から外す。
--
-- 🛑 該当行は**捨てずに 'ratchet_takeprofit' へ戻す**。entry_compensated / external_close
-- (0011 / 0012 の down は DELETE していた)と違い、これは**戦略の出口**でありエッジ標本の
-- 一部なので、down で消すと forward 記録が欠ける。ラベルの粒度だけが失われる。
UPDATE trades SET close_reason = 'ratchet_takeprofit' WHERE close_reason = 'ratchet_giveback_loss';

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated',
     'external_close'));

ALTER TABLE positions DROP COLUMN IF EXISTS ratchet_floor_at_arm;
