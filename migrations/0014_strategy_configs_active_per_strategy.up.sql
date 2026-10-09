-- 0014: active な strategy_config の一意キーを
-- (symbol, mode) → (symbol, mode, strategy_name) へ。
--
-- なぜ: `bnf_reversion` と `bnf_reversion_trail` は `bnfEnter` を共有し**出口だけが違う**
-- のに、**ペアが成立しない**(出口の A/B が設計上のみで標本として成立しない)。
--
-- 原因は 3 重の「銘柄キー」で、そのうち DB 側がこの一意 index。1 銘柄に 1 つしか active
-- config を置けないので、同じトリガーを 2 つのアームが奪い合うだけになる。
--
-- 🛑 **live は 1銘柄1ポジのまま**。緩めるのは paper だけで、
-- その強制はアプリ側(mode: live_config のときは従来の銘柄キー)が持つ。DB の index は
-- mode を含むので、live 行だけを (symbol, mode) で縛る index を別に張って両立させる。
-- ここを 1 本の緩い index にしてしまうと、live の「同一銘柄に複数 active」が DB では
-- 通ってしまい、アプリのバグが素通りする。

DROP INDEX IF EXISTS strategy_configs_active_uidx;

-- paper / backtest 等: 1 銘柄 × 1 戦略につき active は 1 つ。
CREATE UNIQUE INDEX IF NOT EXISTS strategy_configs_active_uidx
    ON strategy_configs (symbol, mode, strategy_name) WHERE status = 'active';

-- live_config だけは従来どおり 1 銘柄 1 active(戦略に依らない)。
CREATE UNIQUE INDEX IF NOT EXISTS strategy_configs_active_live_uidx
    ON strategy_configs (symbol) WHERE status = 'active' AND mode = 'live_config';
