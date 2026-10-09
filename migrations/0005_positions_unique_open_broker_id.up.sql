-- 0005: OPEN/CLOSING の broker_position_id を一意にする(+ 既存の重複を採番し直す)。
--
-- なぜ: paper の建玉帳はプロセス内 map で、採番 seq が毎プロセス 1 から始まっていた。
-- 一方 positions は再起動を跨いで OPEN のまま残るため、再起動のたびに新しい建玉が
-- 古い broker_position_id(bp-2 / bp-6 …)を再利用し、同じ id を持つ OPEN 行が複数
-- できていた。この状態で TP/SL が発火すると ClosePosition が **別銘柄の建玉** を閉じ、
-- その値段で trades に記録される。trades は forward 検証の唯一のエッジ証拠なので、
-- DB 側で構造的に不可能にする。
--
-- 採番の再発防止はアプリ側(cmd/stockbot の adoptPaperBook が起動時に台帳から紙帳簿を
-- 復元し seq を復元 id の先へ進める)で行い、本 index はその最後の砦。

-- 1) 既存の重複を position id 由来の一意な値へ振り直す。元の id は「どの建玉を指すか
--    曖昧」そのものなので保存する価値がなく、紙帳簿は起動時に台帳から作り直されるため
--    この書き換えで失われる情報は無い。idempotent(重複が無ければ 0 行)。
UPDATE positions p
SET broker_position_id = 'paper-' || p.id
WHERE p.status IN ('OPEN', 'CLOSING')
  AND p.broker_position_id IS NOT NULL
  AND p.broker_position_id <> ''
  AND EXISTS (
    SELECT 1 FROM positions q
    WHERE q.status IN ('OPEN', 'CLOSING')
      AND q.broker_position_id = p.broker_position_id
      AND q.id <> p.id
  );

-- 2) 建玉が開いている間、broker_position_id は一意(broker 側の 1 建玉 = 1 id)。
--    CLOSED 行は対象外 — 決済済みの id は誰も参照しないので再利用されても無害。
CREATE UNIQUE INDEX IF NOT EXISTS positions_open_broker_id_uidx
  ON positions (broker_position_id)
  WHERE status IN ('OPEN', 'CLOSING') AND broker_position_id IS NOT NULL AND broker_position_id <> '';
