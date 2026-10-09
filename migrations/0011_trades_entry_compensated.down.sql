-- 逆操作: 'entry_compensated' を CHECK から外す。該当行が残っていると ADD CONSTRAINT が
-- 失敗するので、先に落とす(この close_reason は戦略の出口ではないので消して問題ない)。
DELETE FROM trades WHERE close_reason = 'entry_compensated';
ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat'));
