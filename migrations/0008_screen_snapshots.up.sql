-- 0008: screen_snapshots — LLM ラウンドごとの全銘柄 × 全スクリーナーの screen
-- 結果(triggered / score / 枠を得たか)を永続化する。
--
-- なぜ要るか: advisor_runs には**選ばれた銘柄の run しか残らない**。
-- 「スコアは高かったが枠に入らず見送った銘柄が
-- その後どうなったか」が原理的に検証できず、枠配分ロジック自体の検証(picked
-- vs not-picked のその後の値動き比較)が不可能だった。監査・検定用途のみで、
-- 取引経路からは読まない。
--
-- 容量: 1 ラウンド ≈ 222銘柄 × 8スクリーナー = 1,776 行、1日 ≈ 20 ラウンドで
-- 約 35,000 行/日。retention 方針は docs/workflows/MIGRATIONS.md の 0008 の項。

CREATE TABLE IF NOT EXISTS screen_snapshots (
    id          BIGSERIAL PRIMARY KEY,
    round_at    TIMESTAMPTZ NOT NULL,
    symbol      TEXT        NOT NULL,
    strategy    TEXT        NOT NULL,
    triggered   BOOLEAN     NOT NULL,
    score       DOUBLE PRECISION NOT NULL,
    picked      BOOLEAN     NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS screen_snapshots_round_idx
    ON screen_snapshots (round_at DESC);

CREATE INDEX IF NOT EXISTS screen_snapshots_sym_strat_idx
    ON screen_snapshots (symbol, strategy, round_at DESC);
