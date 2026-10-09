-- 逆操作: backfill した行を `ratchet_takeprofit` へ戻す。
--
-- 🛑 **0013 の down と同じ形**(DELETE ではなく UPDATE)。決済の事実は消さない —
-- ラベルだけを戻す。gross<0 の giveback_loss は 0016 が作ったものと同一集合なので、
-- ここで戻すと 0013 の down(CHECK を 11 値へ戻す)が通る状態になる。
--
-- ⚠ 0016 より後に**新規の** ratchet_giveback_loss が積まれていると、それも
-- ratchet_takeprofit に戻る(= ラベルの嘘が復活する)。rollback は 0013 まで
-- 巻き戻すときだけ使うこと。

UPDATE trades
   SET close_reason = 'ratchet_takeprofit'
 WHERE close_reason = 'ratchet_giveback_loss'
   AND profit_loss_jpy < 0;
