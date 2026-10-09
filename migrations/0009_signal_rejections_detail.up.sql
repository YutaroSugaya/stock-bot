-- 0009: signal_rejections.detail — reason の可変部分(時刻・数量など)を分離する
-- 受け皿。
--
-- これまで reason に "cooldown after_loss until 09:37:25" のように可変部が埋め
-- 込まれ GROUP BY できなかった。0009 以降の挿入は reason = 安定した種別
-- (先頭トークン: cooldown / open_positions / daily_loss …)、detail = 可変部
-- ("after_loss until 09:37:25" 等)に分けて書く。**既存行は書き換えない**
-- (履歴保持 — 旧形式の reason 全文がそのまま残る。detail は '' )。
-- 同時に挿入側はエッジ記録化(同一 symbol × 種別の連続は書かない)される —
-- 詳細は usecase/command/trading_cycle.go。

ALTER TABLE signal_rejections ADD COLUMN IF NOT EXISTS detail TEXT NOT NULL DEFAULT '';
