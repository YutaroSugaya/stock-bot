-- 0020: 多日建玉の時間切れを「N 営業日目の引け前(14:50)」に揃え、
-- **期限日は寄り以降に含み損を観測したらその時点で決済**する(含み益なら 14:50 まで待つ)。
--
-- 背景: 期限が建てた時刻と暦日で決まると、連休明けの寄りや寄りの荒い値で切られる。
-- bnf 家族は 10 暦日 → 10 営業日、表の戦略は営業日数そのまま時刻だけ 14:50 へ。
--
-- positions.max_hold_loss_window_minutes: 期限日の寄りから期限までの窓(分)。**entry 時に凍結**
-- (domain/position は取引時間を知らない)。DEFAULT 0 = このルールが効かない旧建玉。
-- trades.close_reason: 'max_hold_loss' を足す。`max_hold`(14:50 まで持った)と分けて測る。
-- 🛑 **戦略の出口**として数える(port.IsNonStrategyClose に足さない)。
--
-- ⚠ 既存建玉の max_hold_minutes / 窓の書き換えはこの migration では**しない**(建玉ごとの
--   値は暦から計算するので、人間承認の DB 書込で別に当てる)。
-- ⚠ バイナリが読む全 DB に当てる。どれかに無いと決済の書込が CHECK 違反で落ちる。

ALTER TABLE positions ADD COLUMN IF NOT EXISTS max_hold_loss_window_minutes INTEGER NOT NULL DEFAULT 0;

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated',
     'external_close','ratchet_giveback_loss','harvest_expiry','max_hold_loss'));
