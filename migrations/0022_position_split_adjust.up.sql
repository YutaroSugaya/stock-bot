-- 0022: 株式分割(併合)の権利落ちで建玉を言い直すための 2 列と、
-- 分割で誤って決済した往復を記録する決済理由 'split_misfire'。
--
-- 建値・TP・SL が分割前の円で凍結されていると、権利落ち日の寄りで SL を「割った」と読み、
-- 実際は含み益の建玉を大きな見かけの損失で損切りしてしまう。
--
-- positions.split_factor    : 建玉時からの累積の株数倍率(1:5 分割なら 5)。1 = 調整なし。
-- positions.split_adjusted_on: 最後に調整した権利落ち日。**同じ日に二度割らない**ための印
--   (権利落ちの判定は前日終値と時価の比なので、その日のうちは毎ティック・再起動後も成立する)。
--
-- trades.close_reason 'split_misfire': 分割を損切り/利確と誤読して決済した往復。
--   **戦略の出口ではない**(port.IsNonStrategyClose に入れる = エッジ標本から外す)。
--   株数・値段は分割調整後で記録する(経済的な損益は正しい値にする)。
--
-- ⚠ バイナリが読む全 DB に当てる(positions の SELECT がこの 2 列を読む)。

ALTER TABLE positions ADD COLUMN IF NOT EXISTS split_factor DOUBLE PRECISION NOT NULL DEFAULT 1;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS split_adjusted_on DATE;

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated',
     'external_close','ratchet_giveback_loss','harvest_expiry','split_misfire'));
