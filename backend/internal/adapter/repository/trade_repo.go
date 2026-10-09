package repository

import (
	"context"
	"sort"
	"sync"
	"time"

	"stockbot/backend/internal/port"
)

type InMemoryTradeRepo struct {
	mu     sync.Mutex
	nextID int64
	trades []port.TradeRecord
}

func NewInMemoryTradeRepo() *InMemoryTradeRepo { return &InMemoryTradeRepo{} }

func (r *InMemoryTradeRepo) Insert(_ context.Context, t port.TradeRecord) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	t2 := t
	r.trades = append(r.trades, t2)
	return r.nextID, nil
}

// 負けトレードの損失額(正の大きさ)を **net = gross - fee + carry** で測る。手数料は負けを深くするので
// net 合計は gross より **早く** daily loss cap を落とす(保守側)。pg 版と必ず揃える。
func lossMagnitude(t port.TradeRecord) int {
	net := t.ProfitLossJPY - t.FeeJPY + t.CarryJPY
	if net < 0 {
		return int(-net)
	}
	return 0
}

func (r *InMemoryTradeRepo) SumClosedLossJPYSinceBySymbol(_ context.Context, symbol string, since time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sum := 0
	for _, t := range r.trades {
		if t.Symbol == symbol && !t.ClosedAt.Before(since) {
			sum += lossMagnitude(t)
		}
	}
	return sum, nil
}

func (r *InMemoryTradeRepo) CountTradesSinceBySymbol(_ context.Context, symbol string, since time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, t := range r.trades {
		if t.Symbol == symbol && !t.ClosedAt.Before(since) && port.CountsTowardEntryGates(t.CloseReason) {
			n++
		}
	}
	return n, nil
}

func (r *InMemoryTradeRepo) ConsecutiveLossesBySymbol(_ context.Context, symbol string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	streak := 0
	for i := len(r.trades) - 1; i >= 0; i-- {
		t := r.trades[i]
		if t.Symbol != symbol || !port.CountsTowardEntryGates(t.CloseReason) {
			continue
		}
		if t.ProfitLossJPY < 0 {
			streak++
		} else {
			break
		}
	}
	return streak, nil
}

func (r *InMemoryTradeRepo) LastCloseBySymbol(_ context.Context, symbol string) (*port.LastClose, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out *port.LastClose
	for _, t := range r.trades {
		if t.Symbol != symbol || !port.CountsTowardEntryGates(t.CloseReason) {
			continue
		}
		if out == nil || t.ClosedAt.After(out.ClosedAt) {
			out = &port.LastClose{ClosedAt: t.ClosedAt, NetJPY: t.ProfitLossJPY - t.FeeJPY + t.CarryJPY}
		}
	}
	return out, nil
}

// 並びは pg TradeRepo の ORDER BY closed_at, id と揃える。
func (r *InMemoryTradeRepo) ListClosedSince(_ context.Context, since time.Time) ([]port.TradeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]port.TradeRecord, 0, len(r.trades))
	for _, t := range r.trades {
		if !t.ClosedAt.Before(since) {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ClosedAt.Before(out[j].ClosedAt) })
	return out, nil
}

func (r *InMemoryTradeRepo) SumClosedLossJPYSince(_ context.Context, since time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sum := 0
	for _, t := range r.trades {
		if !t.ClosedAt.Before(since) {
			sum += lossMagnitude(t)
		}
	}
	return sum, nil
}

var _ port.TradeRepository = (*InMemoryTradeRepo)(nil)
