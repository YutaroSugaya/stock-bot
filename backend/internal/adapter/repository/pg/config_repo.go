package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"stockbot/backend/internal/port"
)

// positions.config_id の FK があるので、建玉挿入より前に config 行が存在している必要がある。
type ConfigRepo struct{ pool *pgxpool.Pool }

func NewConfigRepo(pool *pgxpool.Pool) *ConfigRepo { return &ConfigRepo{pool: pool} }

func (r *ConfigRepo) EnsureExists(ctx context.Context, rec port.StrategyConfigRecord) error {
	if rec.Status == "" {
		rec.Status = "active"
	}
	if rec.ActivatedAt.IsZero() {
		rec.ActivatedAt = time.Now()
	}
	// config 凍結。既存行は触らない。例外は空 raw_yaml / NULL advisor_run_id の空→非空バックフィルのみ。
	_, err := r.pool.Exec(ctx, `
		INSERT INTO strategy_configs (config_id, symbol, mode, strategy_name, status, raw_yaml, advisor_run_id, activated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (config_id) DO UPDATE SET
			raw_yaml = CASE WHEN strategy_configs.raw_yaml = '' AND EXCLUDED.raw_yaml <> ''
				THEN EXCLUDED.raw_yaml ELSE strategy_configs.raw_yaml END,
			advisor_run_id = COALESCE(strategy_configs.advisor_run_id, EXCLUDED.advisor_run_id)`,
		rec.ConfigID, rec.Symbol, rec.Mode, rec.StrategyName, rec.Status, rec.RawYAML, nullStr(rec.AdvisorRunID), rec.ActivatedAt)
	return err
}

// arm/switch は必ずここを通す。競合する他の active を原子的に expire してから rec を
// active として upsert する — 部分ユニーク索引 strategy_configs_active_uidx の正面口。行の削除も
// raw_yaml の上書きもしない(config 凍結・監査保持: 既存 position は expired 行を FK で参照し続ける)。
// EnsureExists の素 INSERT を arm に使うと active 索引と衝突する: default no_trade(active)と
// ぶつかり、研究モード初日の advisor arm が10件 silent 全滅した。
//
// 🔄 **「競合する」の定義は migration 0014 で変わった**:
//   - paper: (symbol, mode, **strategy_name**) — 1 銘柄に複数戦略の active が並ぶ。
//     ここを (symbol, mode) のままにすると、trail アームを arm した瞬間に capped アームの
//     config が expire し、**両方のアームが同時に走れない**(= 兄弟アームのペア差が取れない)。
//   - live: (symbol, mode) のまま。1銘柄1ポジの不変条件を実弾では緩めない。
func (r *ConfigRepo) ActivateExclusive(ctx context.Context, rec port.StrategyConfigRecord) error {
	if rec.Status == "" {
		rec.Status = "active"
	}
	if rec.ActivatedAt.IsZero() {
		rec.ActivatedAt = time.Now()
	}
	return withTx(ctx, r.pool, func(tx pgx.Tx) error {
		// live は銘柄キー / paper は (銘柄, 戦略) キー。$4 が空文字なら銘柄キー。
		perStrategy := rec.StrategyName
		if rec.Mode == liveConfigMode {
			perStrategy = ""
		}
		if _, err := tx.Exec(ctx, `
			UPDATE strategy_configs SET status='expired'
			WHERE symbol=$1 AND mode=$2 AND status='active' AND config_id <> $3
			  AND ($4 = '' OR strategy_name = $4)`,
			rec.Symbol, rec.Mode, rec.ConfigID, perStrategy); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO strategy_configs (config_id, symbol, mode, strategy_name, status, raw_yaml, advisor_run_id, activated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (config_id) DO UPDATE SET
				status = EXCLUDED.status,
				raw_yaml = CASE WHEN strategy_configs.raw_yaml = '' AND EXCLUDED.raw_yaml <> ''
					THEN EXCLUDED.raw_yaml ELSE strategy_configs.raw_yaml END,
				advisor_run_id = COALESCE(strategy_configs.advisor_run_id, EXCLUDED.advisor_run_id)`,
			rec.ConfigID, rec.Symbol, rec.Mode, rec.StrategyName, rec.Status, rec.RawYAML, nullStr(rec.AdvisorRunID), rec.ActivatedAt)
		return err
	})
}

// liveConfigMode は「一意キーを緩めない側」の mode。**値は config.ModeLive と必ず
// 一致する**(config_repo_mode_test.go が固定する)。文字列で持つのは、この adapter が
// port.StrategyConfigRecord の生の mode 文字列を受け取るため。
const liveConfigMode = "live_config"

// external adoption が FK 参照する番兵 id。
const ExternalConfigID = externalConfigID
