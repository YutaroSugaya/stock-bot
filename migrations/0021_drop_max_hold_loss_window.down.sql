-- 逆操作: 0020 の状態(窓の列 default 0 + CHECK の 'max_hold_loss')へ戻す。
-- 列は default 0 = ルールが効かない値で復元する(値は取り下げ時に捨てた — 0020 は本番未適用)。
ALTER TABLE positions ADD COLUMN IF NOT EXISTS max_hold_loss_window_minutes INTEGER NOT NULL DEFAULT 0;

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated',
     'external_close','ratchet_giveback_loss','harvest_expiry','max_hold_loss'));
