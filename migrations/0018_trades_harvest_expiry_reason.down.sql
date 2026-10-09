-- 逆操作: 'harvest_expiry' を CHECK から外す。
--
-- 🛑 該当行は**捨てずに 'forced_flat' へ戻す**。0013 の down と同じ極性で、決済そのものは
-- 実際に起きた往復なので DELETE すると forward 記録が欠ける。戻し先を `max_hold` に
-- しないのは、あちらが「戦略が決めた期限」を意味するから — 打ち切りを自然決済に
-- 化けさせるくらいなら、粒度を落として「人間が手仕舞った」側(`forced_flat`)へ倒す。
-- 失われるのは「一括手仕舞い」と「日数での打ち切り」の区別だけ。
UPDATE trades SET close_reason = 'forced_flat' WHERE close_reason = 'harvest_expiry';

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated',
     'external_close','ratchet_giveback_loss'));
