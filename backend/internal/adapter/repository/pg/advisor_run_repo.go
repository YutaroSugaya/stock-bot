package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"stockbot/backend/internal/port"
)

// finished_at は nullable(完了しなかった run に終了時刻は無い)。
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// LLM が何を見て何を出したかの監査証跡。観測用のみで取引経路は読まない。
type AdvisorRunRepo struct{ pool *pgxpool.Pool }

func NewAdvisorRunRepo(pool *pgxpool.Pool) *AdvisorRunRepo { return &AdvisorRunRepo{pool: pool} }

// run_id は PK。再挿入は upsert なので best-effort な二重書き込みで呼び手を落とさない。
func (r *AdvisorRunRepo) Insert(ctx context.Context, rec port.AdvisorRunRecord) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO advisor_runs (
			run_id, symbol, status, usage_limited, started_at, finished_at,
			input_json, output_yaml, parsed_yaml, error_msg,
			regime_type, regime_confidence, regime_reason, model)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (run_id) DO UPDATE SET
			status = EXCLUDED.status,
			usage_limited = EXCLUDED.usage_limited,
			finished_at = EXCLUDED.finished_at,
			output_yaml = EXCLUDED.output_yaml,
			parsed_yaml = EXCLUDED.parsed_yaml,
			error_msg = EXCLUDED.error_msg,
			regime_type = EXCLUDED.regime_type,
			regime_confidence = EXCLUDED.regime_confidence,
			regime_reason = EXCLUDED.regime_reason,
			model = EXCLUDED.model`,
		rec.RunID, rec.Symbol, string(rec.Status), rec.UsageLimited, rec.StartedAt, nullTime(rec.FinishedAt),
		rec.InputJSON, rec.OutputYAML, rec.ParsedYAML, rec.ErrorMsg,
		rec.RegimeType, rec.RegimeConfidence, rec.RegimeReason, rec.Model)
	return err
}

func (r *AdvisorRunRepo) List(ctx context.Context, symbol string, limit int) ([]port.AdvisorRunRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
		SELECT run_id, symbol, status, usage_limited, started_at, finished_at,
		       input_json, output_yaml, parsed_yaml, error_msg,
		       regime_type, regime_confidence, regime_reason, model
		  FROM advisor_runs
		 WHERE ($1 = '' OR symbol = $1)
		 ORDER BY started_at DESC
		 LIMIT $2`, symbol, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []port.AdvisorRunRecord
	for rows.Next() {
		var rec port.AdvisorRunRecord
		var status string
		var finished *time.Time
		if err := rows.Scan(&rec.RunID, &rec.Symbol, &status, &rec.UsageLimited, &rec.StartedAt, &finished,
			&rec.InputJSON, &rec.OutputYAML, &rec.ParsedYAML, &rec.ErrorMsg,
			&rec.RegimeType, &rec.RegimeConfidence, &rec.RegimeReason, &rec.Model); err != nil {
			return nil, err
		}
		rec.Status = port.AdvisorRunStatus(status)
		if finished != nil {
			rec.FinishedAt = *finished
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

var _ port.AdvisorRunRepository = (*AdvisorRunRepo)(nil)
