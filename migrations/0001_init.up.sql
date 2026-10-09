-- 0001_init — initial schema. Forward-looking schema-as-code for
-- the production Postgres adapter (pgx/sqlc). The in-memory repos mirror this
-- shape; this migration is the SSOT for the eventual DB. up is idempotent.
-- Forward ALTER only; never edit an applied migration.

CREATE TABLE IF NOT EXISTS strategy_configs (
    config_id      TEXT PRIMARY KEY,
    symbol         TEXT NOT NULL,
    mode           TEXT NOT NULL,
    strategy_name  TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'active',  -- active | expired | rejected
    raw_yaml       TEXT NOT NULL,
    activated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- one active config per (symbol, mode)
CREATE UNIQUE INDEX IF NOT EXISTS strategy_configs_active_uidx
    ON strategy_configs (symbol, mode) WHERE status = 'active';

CREATE TABLE IF NOT EXISTS positions (
    id                  BIGSERIAL PRIMARY KEY,
    broker_position_id  TEXT,
    symbol              TEXT NOT NULL,
    side                TEXT NOT NULL CHECK (side IN ('BUY','SELL')),
    quantity            INTEGER NOT NULL,
    entry_price         DOUBLE PRECISION NOT NULL,
    take_profit_ticks   DOUBLE PRECISION NOT NULL DEFAULT 0,
    stop_loss_ticks     DOUBLE PRECISION NOT NULL DEFAULT 0,
    max_hold_minutes    INTEGER NOT NULL DEFAULT 0,
    ratchet_arm_ticks       DOUBLE PRECISION NOT NULL DEFAULT 0,
    ratchet_giveback_ticks  DOUBLE PRECISION NOT NULL DEFAULT 0,
    peak_unrealized_ticks   DOUBLE PRECISION NOT NULL DEFAULT 0,
    ratchet_armed           BOOLEAN NOT NULL DEFAULT false,
    config_id           TEXT NOT NULL REFERENCES strategy_configs(config_id),
    holding_mode        TEXT NOT NULL,   -- intraday | multiday (株固有)
    exec_kind           TEXT NOT NULL,   -- cash | margin_oneday | margin_general (株固有)
    tick_size_at_entry  DOUBLE PRECISION NOT NULL DEFAULT 0,  -- 株固有
    source              TEXT NOT NULL DEFAULT 'bot',  -- bot | manual | external_broker
    entry_fee_jpy       DOUBLE PRECISION,             -- nil=未取得 / 0=broker報告0
    status              TEXT NOT NULL CHECK (status IN ('OPEN','CLOSING','CLOSED')),
    opened_at           TIMESTAMPTZ NOT NULL,
    closed_at           TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS positions_open_idx ON positions (symbol, status);

-- broker-side protective leg ids (junction; NULL-avoidance)
CREATE TABLE IF NOT EXISTS positions_live (
    position_id         BIGINT PRIMARY KEY REFERENCES positions(id),
    broker_position_id  TEXT NOT NULL,
    tp_order_id         TEXT,
    sl_order_id         TEXT
);

CREATE TABLE IF NOT EXISTS trades (
    id              BIGSERIAL PRIMARY KEY,
    position_id     BIGINT NOT NULL REFERENCES positions(id),
    symbol          TEXT NOT NULL,
    side            TEXT NOT NULL,
    quantity        INTEGER NOT NULL,
    entry_price     DOUBLE PRECISION NOT NULL,
    close_price     DOUBLE PRECISION NOT NULL,
    profit_loss_jpy DOUBLE PRECISION NOT NULL,         -- GROSS
    fee_jpy         DOUBLE PRECISION NOT NULL DEFAULT 0, -- 往復・正=コスト
    carry_jpy       DOUBLE PRECISION NOT NULL DEFAULT 0, -- 配当落調整/貸株料/金利・受取=正
    fee_estimated   BOOLEAN NOT NULL DEFAULT false,
    close_reason    TEXT NOT NULL CHECK (close_reason IN
        ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
         'manual','reconcile_cold_close','broker_close','forced_flat')),
    closed_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS trades_symbol_closed_idx ON trades (symbol, closed_at);

CREATE TABLE IF NOT EXISTS candles (
    symbol     TEXT NOT NULL,
    interval   TEXT NOT NULL,   -- 1m|5m|1h|1d
    open_time  TIMESTAMPTZ NOT NULL,
    open       DOUBLE PRECISION NOT NULL,
    high       DOUBLE PRECISION NOT NULL,
    low        DOUBLE PRECISION NOT NULL,
    close      DOUBLE PRECISION NOT NULL,
    volume     DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (symbol, interval, open_time)
);

CREATE TABLE IF NOT EXISTS signal_rejections (
    id          BIGSERIAL PRIMARY KEY,
    symbol      TEXT NOT NULL,
    config_id   TEXT,
    reason      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
