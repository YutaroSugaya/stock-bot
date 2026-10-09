package pg

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"stockbot/backend/internal/port"
)

// 1ラウンドが 200銘柄 × 8スクリーナー規模なので行単位 INSERT ではなく COPY で1往復に畳む。
type ScreenSnapshotRepo struct{ pool *pgxpool.Pool }

func NewScreenSnapshotRepo(pool *pgxpool.Pool) *ScreenSnapshotRepo {
	return &ScreenSnapshotRepo{pool: pool}
}

func (r *ScreenSnapshotRepo) InsertRound(ctx context.Context, rows []port.ScreenSnapshot) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := r.pool.CopyFrom(ctx,
		pgx.Identifier{"screen_snapshots"},
		[]string{"round_at", "symbol", "strategy", "triggered", "score", "picked", "side"},
		pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
			s := rows[i]
			return []any{s.RoundAt, s.Symbol, s.Strategy, s.Triggered, s.Score, s.Picked, s.Side}, nil
		}))
	return err
}

var _ port.ScreenSnapshotRepository = (*ScreenSnapshotRepo)(nil)
