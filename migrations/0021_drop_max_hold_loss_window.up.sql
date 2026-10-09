-- 0021: 0020 の「期限日は寄り以降に含み損なら先に切る」を**取り下げる**。
-- 時間切れは損益に依らず期限日の 14:50 だけ(N 営業日目の 14:50 は残す — それは列を持たない)。
--
-- 取り下げの理由: ①寄りは一日で一番荒い時間帯で、そこで切る根拠が無い ②平均回帰に賭ける bnf で
-- 「含み損だから数時間早く切る」は戦略の前提と矛盾する(悪化の防御は板の SL の役割)
-- ③出口が 2 種類に増え、未検定の枝が 1 本増える。
--
-- `max_hold_loss` の行は存在しない想定。念のため
-- 行があれば `max_hold` へ戻してから CHECK を狭める(DELETE しない)。
UPDATE trades SET close_reason = 'max_hold' WHERE close_reason = 'max_hold_loss';

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated',
     'external_close','ratchet_giveback_loss','harvest_expiry'));

ALTER TABLE positions DROP COLUMN IF EXISTS max_hold_loss_window_minutes;
