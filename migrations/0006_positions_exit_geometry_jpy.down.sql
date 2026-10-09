-- 0006 down: tick 建ての列を戻し、円幅を呼値で割って復元する。
-- tick_size_at_entry が 0 の行は割れないので 0(= その脚は無い)。
-- take_profit_price / stop_loss_price に対応する列は元スキーマに無いので落とす。

ALTER TABLE positions ADD COLUMN IF NOT EXISTS take_profit_ticks             double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS stop_loss_ticks               double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS ratchet_arm_ticks             double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS ratchet_giveback_ticks        double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS extension_unrealized_threshold double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS early_exit_target_ticks       double precision NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS peak_unrealized_ticks         double precision NOT NULL DEFAULT 0;

UPDATE positions SET
	take_profit_ticks             = take_profit_jpy / tick_size_at_entry,
	stop_loss_ticks               = stop_loss_jpy / tick_size_at_entry,
	ratchet_arm_ticks             = ratchet_arm_jpy / tick_size_at_entry,
	ratchet_giveback_ticks        = ratchet_giveback_jpy / tick_size_at_entry,
	extension_unrealized_threshold = extension_unrealized_jpy / tick_size_at_entry,
	early_exit_target_ticks       = early_exit_target_jpy / tick_size_at_entry,
	peak_unrealized_ticks         = peak_unrealized_jpy / tick_size_at_entry
WHERE tick_size_at_entry > 0;

ALTER TABLE positions DROP COLUMN IF EXISTS take_profit_jpy;
ALTER TABLE positions DROP COLUMN IF EXISTS stop_loss_jpy;
ALTER TABLE positions DROP COLUMN IF EXISTS take_profit_price;
ALTER TABLE positions DROP COLUMN IF EXISTS stop_loss_price;
ALTER TABLE positions DROP COLUMN IF EXISTS ratchet_arm_jpy;
ALTER TABLE positions DROP COLUMN IF EXISTS ratchet_giveback_jpy;
ALTER TABLE positions DROP COLUMN IF EXISTS extension_unrealized_jpy;
ALTER TABLE positions DROP COLUMN IF EXISTS early_exit_target_jpy;
ALTER TABLE positions DROP COLUMN IF EXISTS peak_unrealized_jpy;
