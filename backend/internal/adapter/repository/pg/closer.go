package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"stockbot/backend/internal/port"
)

// CLOSED への遷移と trade 行の挿入を1トランザクションで行う。CLOSING でなければ ok=false。
type Closer struct{ pool *pgxpool.Pool }

func NewCloser(pool *pgxpool.Pool) *Closer { return &Closer{pool: pool} }

func (c *Closer) CloseAndRecord(ctx context.Context, positionID int64, closedAt time.Time, trade port.TradeRecord) (bool, error) {
	ok := false
	err := withTx(ctx, c.pool, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `UPDATE positions SET status='CLOSED', closed_at=$2 WHERE id=$1 AND status='CLOSING'`, positionID, closedAt)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return nil // benign skip: not CLOSING / already closed → ok stays false
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO trades (position_id, symbol, side, quantity, entry_price, close_price,
				profit_loss_jpy, fee_jpy, carry_jpy, fee_estimated, close_reason, closed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			trade.PositionID, trade.Symbol, string(trade.Side), trade.Quantity, trade.EntryPrice, trade.ClosePrice,
			trade.ProfitLossJPY, trade.FeeJPY, trade.CarryJPY, trade.FeeEstimated, trade.CloseReason, trade.ClosedAt)
		if err != nil {
			return err
		}
		ok = true
		return nil
	})
	return ok, err
}

var _ port.PositionCloser = (*Closer)(nil)
