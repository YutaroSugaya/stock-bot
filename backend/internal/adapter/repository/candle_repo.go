package repository

import (
	"context"
	"sort"
	"sync"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

type InMemoryCandleRepo struct {
	mu    sync.Mutex
	store map[string][]market.Candle
}

func NewInMemoryCandleRepo() *InMemoryCandleRepo {
	return &InMemoryCandleRepo{store: make(map[string][]market.Candle)}
}

func candleKey(symbol string, p port.KlinePeriod) string { return symbol + "|" + string(p) }

// open_time で dedupe し、衝突時は既存を残す(pg の ON CONFLICT DO NOTHING と同じ = 定期再取得が冪等)。
func (r *InMemoryCandleRepo) Upsert(_ context.Context, symbol string, candles []market.Candle) error {
	if len(candles) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := periodForInterval(candles[0])
	key := candleKey(symbol, p)
	merged := r.store[key]
	// 🛑 同一 open_time は **上書き**する(pg 側の ON CONFLICT DO UPDATE と同じ規約)。
	// 株式分割は「過去のバーを遡って書き換える」操作なので、DO NOTHING だと
	// chain-link 済みの調整値が永久に入らない。
	at := make(map[int64]int, len(merged))
	for i, c := range merged {
		at[c.OpenTime.UnixNano()] = i
	}
	for _, c := range candles {
		k := c.OpenTime.UnixNano()
		if i, dup := at[k]; dup {
			merged[i] = c
			continue
		}
		at[k] = len(merged)
		merged = append(merged, c)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].OpenTime.Before(merged[j].OpenTime) })
	r.store[key] = merged
	return nil
}

func (r *InMemoryCandleRepo) List(_ context.Context, symbol string, period port.KlinePeriod, limit int) ([]market.Candle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := r.store[candleKey(symbol, period)]
	if limit > 0 && limit < len(all) {
		all = all[len(all)-limit:]
	}
	return append([]market.Candle(nil), all...), nil
}

func periodForInterval(c market.Candle) port.KlinePeriod {
	switch c.Interval.Minutes() {
	case 1:
		return port.Period1m
	case 5:
		return port.Period5m
	case 60:
		return port.Period1h
	default:
		return port.PeriodDaily
	}
}

var _ port.CandleRepository = (*InMemoryCandleRepo)(nil)
