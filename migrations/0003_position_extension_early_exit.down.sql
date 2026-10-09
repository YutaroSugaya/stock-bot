-- 0003_position_extension_early_exit (down) — exact inverse: drop the four
-- columns. Reverting also re-disables the extension / early-exit exit branches
-- under Postgres, so only roll back together with the code that stopped writing
-- them.

ALTER TABLE positions
    DROP COLUMN IF EXISTS early_exit_target_ticks,
    DROP COLUMN IF EXISTS early_exit_window_minutes,
    DROP COLUMN IF EXISTS extension_unrealized_threshold,
    DROP COLUMN IF EXISTS extension_max_minutes;
