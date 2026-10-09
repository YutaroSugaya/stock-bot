package pg

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"stockbot/backend/internal/port"
)

// 「なぜエントリーしなかったか」の永続監査証跡。
type RejectionRepo struct{ pool *pgxpool.Pool }

func NewRejectionRepo(pool *pgxpool.Pool) *RejectionRepo { return &RejectionRepo{pool: pool} }

// config_id は空なら NULL(config 前の reject も記録する)。reason は安定種別・detail は可変部。
func (r *RejectionRepo) InsertRejection(ctx context.Context, rej port.SignalRejection) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO signal_rejections (symbol, config_id, reason, detail, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		rej.Symbol, nullStr(rej.ConfigID), rej.Reason, rej.Detail, rej.CreatedAt)
	return err
}

var _ port.SignalRejectionRepository = (*RejectionRepo)(nil)
