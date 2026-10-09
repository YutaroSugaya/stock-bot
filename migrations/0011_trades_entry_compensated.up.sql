-- trades.close_reason に 'entry_compensated' を足す。
--
-- 約定した**後**に守り(OCO)を board に置けず、
-- entry saga が補償で建玉を閉じたとき、これまでは positions にも trades にも 1 行も
-- 残らなかった。再入場を止めうるゲート(cooldown / max_trades_in_this_window /
-- daily_loss / account_daily_loss / consecutive_losses)は**全部が台帳を読む**ので、
-- 台帳が空だと同時に無効化され、同じ銘柄を何往復もしてしまう。
--
-- 戦略の出口ではないので、エッジ判定では close_reason で除外できる形にしておく。
ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated'));
