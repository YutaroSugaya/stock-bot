-- 0022 の逆操作。'split_misfire' の行は、訂正前にその往復へ付いていた 'stop_loss' へ戻してから
-- CHECK を狭める(DELETE しない — 台帳は forward 検証の唯一の証拠)。
UPDATE trades SET close_reason = 'stop_loss' WHERE close_reason = 'split_misfire';

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated',
     'external_close','ratchet_giveback_loss','harvest_expiry'));

ALTER TABLE positions DROP COLUMN IF EXISTS split_adjusted_on;
ALTER TABLE positions DROP COLUMN IF EXISTS split_factor;
