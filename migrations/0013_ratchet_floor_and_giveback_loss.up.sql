-- 0013: トレールに「利益を確定する床」を入れる + ラベルの嘘を直す。
--
-- (1) positions.ratchet_floor_at_arm
--
-- 新規則 floor = max(ratchet_arm_jpy, peak − ratchet_giveback_jpy) は Position に凍結された
-- 値から**実行時に**計算されるので、フラグが無いとコードを変えた瞬間に
-- **既に開いている建玉にも効いてしまう**(測っている対象そのものを途中で入れ替えることになる)。
--
-- したがって **default false = 既存行は旧規則のまま**。新規建玉だけが true で入る。
--
-- (2) trades.close_reason に 'ratchet_giveback_loss' を足す
--
-- `ratchet_takeprofit` が**損失で出る**ことがあった。
-- 段2(LLM)も台帳も「利確が 3 回発火した」と読む。gross(fee/carry 前)の符号で分ける
-- ——「決済値 vs 建値」の価格比較では SELL 建玉で逆になるので使わない。
-- **どちらも戦略の出口**なのでエッジ標本に入れる(entry_compensated / external_close
-- とは扱いが違う。port.IsNonStrategyClose には足さない)。
--
-- ⚠ バイナリが読む全 DB に当てる(どれかにこの列が無いと起動時に落ちる)。

ALTER TABLE positions
    ADD COLUMN IF NOT EXISTS ratchet_floor_at_arm BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated',
     'external_close','ratchet_giveback_loss'));
