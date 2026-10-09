-- 0016: 0013 が足した `ratchet_giveback_loss` を
-- **既存行に backfill する**。
--
-- 0013 は CHECK 制約に値を足しただけで、**動機になった既存行を直していなかった**
-- (down だけが UPDATE で戻すという非対称)。そのため「`ratchet_takeprofit` が損失で
-- 出ていた」行が台帳に**そのまま残っていた**。
--
-- 判定は **gross(fee/carry 前 = profit_loss_jpy)の符号**。`RatchetCloseReason` と
-- 同じ規則で、gross < 0 のときだけ giveback_loss に落とす。
--   - 「決済値 vs 建値」の価格比較は SELL 建玉で逆になるので使わない。
--   - gross == 0 は **`ratchet_takeprofit` のまま**(定義どおり。損失ではない)。
--
-- ⚠ バイナリが読む全 DB に当てる。
-- 冪等: 対象は close_reason='ratchet_takeprofit' かつ gross<0 の行だけなので、
-- 2 回当てても 2 回目は 0 行。

UPDATE trades
   SET close_reason = 'ratchet_giveback_loss'
 WHERE close_reason = 'ratchet_takeprofit'
   AND profit_loss_jpy < 0;
