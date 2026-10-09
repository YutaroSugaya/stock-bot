-- positions_live (broker-side protective leg ids) was write-only: the cancel
-- path reads the board (ListProtectiveOrders) and nothing read
-- the table back. Dropping it removes a copy that could only drift from the board.
DROP TABLE IF EXISTS positions_live;
