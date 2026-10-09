-- 0018: trades.close_reason に 'harvest_expiry' を足す。
--
-- 収穫トラック(自然決済を待つだけの残玉)は config 凍結で `max_hold_minutes = 0` = **無期限**で、
-- 出口が板の TP/SL しか無い。全部消えるまで長期間、監視銘柄を占有し、共有の時価取得の
-- 間隔を押し上げる。
--
-- そこで bot config の `holding.max_hold_override_days` で
-- **トラック側から**期限を課す。この migration はその決済を記録する理由を足すだけ。
--
-- 🛑 なぜ `max_hold` に相乗りしないか: あちらは「建玉時にその戦略が決めた出口」で、
--    こちらは **人間が後から課した打ち切り = 検閲標本**。同じ理由で書くと、エッジ判定
--    (cmd/edge-judge)も保有期間分布(cmd/holding-period)も検閲を自然決済として数える
-- 🛑 凍結値 `positions.max_hold_minutes` は**書き換えない**(config 凍結の不変条件)。
--    打ち切りは決済側の理由だけで表現する。
-- 🛑 **戦略の出口としては数える** = `port.IsNonStrategyClose` には足さない
--    (forced_flat / max_hold と同じ扱い)。検閲であることは理由の名前と
--    `cmd/counterfactual`(既定の -reasons に入れてある)で読む。
--
-- ⚠ バイナリが読む全 DB に当てる(どれかに値が無いと決済の書込が CHECK 違反で落ちる)。

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated',
     'external_close','ratchet_giveback_loss','harvest_expiry'));
