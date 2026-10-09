-- Recreate the junction table exactly as 0001 defined it.
CREATE TABLE IF NOT EXISTS positions_live (
    position_id         BIGINT PRIMARY KEY REFERENCES positions(id),
    broker_position_id  TEXT NOT NULL,
    tp_order_id         TEXT,
    sl_order_id         TEXT
);
