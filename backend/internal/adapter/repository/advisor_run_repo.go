package repository

import (
	"context"
	"sort"
	"sync"

	"stockbot/backend/internal/port"
)

// 観測用のみ。取引経路はここを読まない。
type InMemoryAdvisorRunRepo struct {
	mu   sync.RWMutex
	rows []port.AdvisorRunRecord
}

func NewInMemoryAdvisorRunRepo() *InMemoryAdvisorRunRepo { return &InMemoryAdvisorRunRepo{} }

// 同じ run_id の再挿入は置換(PK 相当)。
func (r *InMemoryAdvisorRunRepo) Insert(_ context.Context, rec port.AdvisorRunRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.rows {
		if existing.RunID == rec.RunID {
			r.rows[i] = rec
			return nil
		}
	}
	r.rows = append(r.rows, rec)
	return nil
}

// 新しい順。symbol "" は全件、limit <= 0 は無制限。
func (r *InMemoryAdvisorRunRepo) List(_ context.Context, symbol string, limit int) ([]port.AdvisorRunRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]port.AdvisorRunRecord, 0, len(r.rows))
	for _, rec := range r.rows {
		if symbol == "" || rec.Symbol == symbol {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

var _ port.AdvisorRunRepository = (*InMemoryAdvisorRunRepo)(nil)
