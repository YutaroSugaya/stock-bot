package repository

import (
	"context"
	"sync"

	"stockbot/backend/internal/port"
)

type InMemoryRejectionRepo struct {
	mu   sync.Mutex
	rows []port.SignalRejection
}

func NewInMemoryRejectionRepo() *InMemoryRejectionRepo { return &InMemoryRejectionRepo{} }

func (r *InMemoryRejectionRepo) InsertRejection(_ context.Context, rej port.SignalRejection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, rej)
	return nil
}

func (r *InMemoryRejectionRepo) All() []port.SignalRejection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]port.SignalRejection(nil), r.rows...)
}

var _ port.SignalRejectionRepository = (*InMemoryRejectionRepo)(nil)
