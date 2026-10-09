package repository

import (
	"context"
	"time"

	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// pg の withTx close saga と同じ原子性を position repo のロックで作る。CLOSING でなければ ok=false。
type Closer struct {
	pos    *InMemoryPositionRepo
	trades *InMemoryTradeRepo
}

func NewCloser(pos *InMemoryPositionRepo, trades *InMemoryTradeRepo) *Closer {
	return &Closer{pos: pos, trades: trades}
}

func (c *Closer) CloseAndRecord(ctx context.Context, positionID int64, closedAt time.Time, trade port.TradeRecord) (bool, error) {
	c.pos.mu.Lock()
	p, ok := c.pos.byID[positionID]
	if !ok || p.Status != position.StatusClosing {
		c.pos.mu.Unlock()
		return false, nil // benign skip: not claimed / already closed
	}
	p.Status = position.StatusClosed
	t := closedAt
	p.ClosedAt = &t
	c.pos.mu.Unlock()

	if _, err := c.trades.Insert(ctx, trade); err != nil {
		return false, err
	}
	return true, nil
}

var _ port.PositionCloser = (*Closer)(nil)
