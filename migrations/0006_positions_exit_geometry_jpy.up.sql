-- 0006: 建玉の出口幾何を tick 建てから **円/株** 建てへ移す(+ 確定した絶対価格を保存)。
--
-- なぜ: 東証の呼値は銘柄(TOPIX500 か否か)と価格帯で 0.1〜10円に変わる。TP/SL を
-- tick 数で書くと、同じ config が銘柄ごとに別の賭けになる(実測: 同じ TP=100tick が
-- ある銘柄で +1,000円、別の銘柄で +50,000円)。呼値は「発注価格をグリッドに丸める」
-- ためだけに使い、幾何そのものは円で持つ。
--
-- take_profit_price / stop_loss_price は約定値に幅を足して確定した絶対価格。
-- config は arm 時点・entry はその後なので幅で持ち、建玉時に価格へ確定させる。
-- broker の OCO に入っている値段と同じものが行に残るので、UI も判定もこれを読む。

ALTER TABLE positions ADD COLUMN IF NOT EXISTS take_profit_jpy          double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS stop_loss_jpy            double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS take_profit_price        double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS stop_loss_price          double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS ratchet_arm_jpy          double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS ratchet_giveback_jpy     double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS extension_unrealized_jpy double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS early_exit_target_jpy    double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS peak_unrealized_jpy      double precision NOT NULL DEFAULT 0;

-- 既存行の換算: tick 数 × 建玉時の呼値 = 円幅。tick_size_at_entry が 0 の行
-- (外部採用など)は換算できないので 0 のまま = 「その脚は無い」。
-- 絶対価格は建値 ± 幅(買いは TP 上・SL 下、売りは逆)。
UPDATE positions SET
	take_profit_jpy          = take_profit_ticks * tick_size_at_entry,
	stop_loss_jpy            = stop_loss_ticks * tick_size_at_entry,
	ratchet_arm_jpy          = ratchet_arm_ticks * tick_size_at_entry,
	ratchet_giveback_jpy     = ratchet_giveback_ticks * tick_size_at_entry,
	extension_unrealized_jpy = extension_unrealized_threshold * tick_size_at_entry,
	early_exit_target_jpy    = early_exit_target_ticks * tick_size_at_entry,
	peak_unrealized_jpy      = peak_unrealized_ticks * tick_size_at_entry
WHERE tick_size_at_entry > 0;

UPDATE positions SET
	take_profit_price = CASE WHEN take_profit_jpy > 0 THEN
		CASE WHEN side = 'SELL' THEN entry_price - take_profit_jpy ELSE entry_price + take_profit_jpy END
		ELSE 0 END,
	stop_loss_price = CASE WHEN stop_loss_jpy > 0 THEN
		CASE WHEN side = 'SELL' THEN entry_price + stop_loss_jpy ELSE entry_price - stop_loss_jpy END
		ELSE 0 END;

ALTER TABLE positions DROP COLUMN IF EXISTS take_profit_ticks;
ALTER TABLE positions DROP COLUMN IF EXISTS stop_loss_ticks;
ALTER TABLE positions DROP COLUMN IF EXISTS ratchet_arm_ticks;
ALTER TABLE positions DROP COLUMN IF EXISTS ratchet_giveback_ticks;
ALTER TABLE positions DROP COLUMN IF EXISTS extension_unrealized_threshold;
ALTER TABLE positions DROP COLUMN IF EXISTS early_exit_target_ticks;
ALTER TABLE positions DROP COLUMN IF EXISTS peak_unrealized_ticks;
