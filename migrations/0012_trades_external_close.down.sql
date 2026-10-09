-- 逆操作: 'external_close' を CHECK から外す。該当行が残っていると ADD CONSTRAINT が
-- 失敗するので先に落とす(戦略の出口ではないので消して差し支えない)。
DELETE FROM trades WHERE close_reason = 'external_close';
ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated'));
