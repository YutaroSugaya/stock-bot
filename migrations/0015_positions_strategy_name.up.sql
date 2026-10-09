-- 0015: positions に strategy_name を凍結する。
--
-- ナンピン禁止のキーが (銘柄, 側) → (銘柄, 側, **戦略**) になったので、entry のたびに
-- 「この銘柄で**同じ戦略**の建玉が既に開いているか」を答える必要がある。建玉から戦略名を
-- 引く既存の手段は strategy_configs への join(`StrategyByPositionID`)だけで、
-- **エントリー経路(3秒ごと × 200銘柄)で毎回 join を足すのは払えない**。
--
-- 出口幾何・holding_mode・exec_kind と同じ **config 凍結の一部**として非正規化する
-- (positions は既にそういう表)。config_id への FK は残るので、食い違いは監査で検出できる。
--
-- default '' = **戦略不明**。既存行と external 建玉がこれになる。
-- 🛑 戦略不明はナンピン判定で「どの戦略に対しても数える」側に倒す — 空文字を
-- 「戦略が違う」と読むと、external 建玉や旧建玉と二重に持つ。
ALTER TABLE positions
    ADD COLUMN IF NOT EXISTS strategy_name TEXT NOT NULL DEFAULT '';

-- 既存行の backfill: 凍結 config から引ける分だけ埋める(推測しない)。
-- idempotent — 既に埋まっている行は触らない。
UPDATE positions p
SET strategy_name = sc.strategy_name
FROM strategy_configs sc
WHERE p.config_id = sc.config_id AND p.strategy_name = '';

-- ナンピン判定は (symbol, status, side) で開いている建玉を引くので、既存の
-- positions_open_idx (symbol, status) で足りる。戦略別の index は足さない
-- (1 銘柄の OPEN は多くて戦略数ぶん = 十数行)。
