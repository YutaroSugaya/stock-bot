package repository

import (
	"context"
	"sync"

	"stockbot/backend/internal/port"
)

type InMemoryScreenSnapshotRepo struct {
	mu   sync.Mutex
	rows []port.ScreenSnapshot
}

func NewInMemoryScreenSnapshotRepo() *InMemoryScreenSnapshotRepo {
	return &InMemoryScreenSnapshotRepo{}
}

func (r *InMemoryScreenSnapshotRepo) InsertRound(_ context.Context, rows []port.ScreenSnapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, rows...)
	return nil
}

func (r *InMemoryScreenSnapshotRepo) All() []port.ScreenSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]port.ScreenSnapshot(nil), r.rows...)
}

var _ port.ScreenSnapshotRepository = (*InMemoryScreenSnapshotRepo)(nil)
