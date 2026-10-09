-- 逆操作: 'max_hold_loss' を CHECK から外し、窓の列を落とす。
--
-- 🛑 該当行は**捨てずに 'max_hold' へ戻す**。どちらも戦略が決めた時間切れで、失われるのは
-- 「期限日に含み損で先に切った」と「14:50 まで持った」の区別だけ(DELETE すると forward
-- 記録が欠ける)。
UPDATE trades SET close_reason = 'max_hold' WHERE close_reason = 'max_hold_loss';

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated',
     'external_close','ratchet_giveback_loss','harvest_expiry'));

ALTER TABLE positions DROP COLUMN IF EXISTS max_hold_loss_window_minutes;
