package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/adapter/repository/pg"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/port"
)

// store bundles the persistence ports plus an optional config-row ensurer
// (Postgres only, to satisfy the positions.config_id FK) and a close func.
type store struct {
	positions   port.PositionRepository
	trades      port.TradeRepository
	closer      port.PositionCloser
	candles     port.CandleRepository
	rejections  port.SignalRejectionRepository
	advisorRuns port.AdvisorRunRepository
	screens     port.ScreenSnapshotRepository
	strategies  port.TradeStrategyResolver // nil in memory mode: 戦略別の内訳が出ないだけ
	// activateCfg atomically expires the previous active config for the same
	// (symbol, mode) and activates rec — 部分ユニーク索引 strategy_configs_active_uidx
	// と整合する arm/切替の正面口。nil in memory mode。
	activateCfg func(rec port.StrategyConfigRecord) error
	backend     string
	closeFn     func()
}

// buildLiveStore builds the live track's persistence. **in-memory フォールバックは
// 持たない** — buildStore は DSN 未設定で in-memory に落ちるので、live がそれを流用
// すると「記録が消える live」が静かに成立する(再起動で daily-loss 台帳・凍結 config・
// 建玉が全部飛ぶ)。DSN が無いなら起動しない、が唯一正しい振る舞い。
//
// research と**別の pgxpool** になるが、接続数は小さいので問題にならない。分けること
// 自体が目的 — 同一 DB + track 列は paper を live 成績として読む事故面が広い。
func buildLiveStore(ctx context.Context, dsn string, logger *slog.Logger) (store, error) {
	if dsn == "" {
		return store{}, fmt.Errorf("live track には STOCKBOT_LIVE_DATABASE_URL が必要(live に in-memory フォールバックは無い)")
	}
	st, err := buildStore(ctx, config.Env{DatabaseURL: dsn}, logger)
	if err != nil {
		return store{}, err
	}
	if st.backend != "postgres" {
		// 到達しないはずだが、buildStore 側の分岐が将来変わったときに黙って
		// in-memory の live が生まれるのを防ぐ。
		return store{}, fmt.Errorf("live track の永続化が %q になった(postgres 以外は不可)", st.backend)
	}
	return st, nil
}

// buildStore selects Postgres when STOCKBOT_DATABASE_URL is set, else the
// in-memory repos. The schema must already be migrated (cmd/migrate) — main
// never migrates implicitly.
//
// Startup DB work uses a dedicated background context, NOT the caller's
// signal-cancellable one, so an early SIGINT during setup cannot abort the
// FK-ensure mid-flight.
func buildStore(_ context.Context, env config.Env, logger *slog.Logger) (store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if env.DatabaseURL == "" {
		posRepo := repository.NewInMemoryPositionRepo()
		tradeRepo := repository.NewInMemoryTradeRepo()
		return store{
			positions:   posRepo,
			trades:      tradeRepo,
			closer:      repository.NewCloser(posRepo, tradeRepo),
			candles:     repository.NewInMemoryCandleRepo(),
			rejections:  repository.NewInMemoryRejectionRepo(),
			advisorRuns: repository.NewInMemoryAdvisorRunRepo(),
			screens:     repository.NewInMemoryScreenSnapshotRepo(),
			backend:     "memory",
			closeFn:     func() {},
		}, nil
	}

	pool, err := pg.Open(ctx, env.DatabaseURL)
	if err != nil {
		return store{}, fmt.Errorf("postgres: %w", err)
	}
	// 🛑 **どの台帳に書くかを起動時に名指しする**。DSN の切り替えを
	// 忘れたまま起動すると、新しい設定の arm 挙動が前の台帳へ書き込まれる。
	// DSN の取り違えを落とす検査は live トラックの別 DB 判定だけなので、
	// research 単体の取り違えには誰も気づけない —— せめて 1 行目に出す。
	// 認証情報は出さない(ログは journal に残る)。
	logger.Info("using postgres persistence", "database", dsnDatabaseName(env.DatabaseURL))
	posRepo := pg.NewPositionRepo(pool)
	cfgRepo := pg.NewConfigRepo(pool)
	// External-adoption sentinel config row (FK target for adopted positions).
	if err := cfgRepo.EnsureExists(ctx, port.StrategyConfigRecord{
		ConfigID: pg.ExternalConfigID, Symbol: "*", Mode: "system", StrategyName: "external", Status: "active", RawYAML: "",
	}); err != nil {
		pool.Close()
		return store{}, fmt.Errorf("postgres: ensure external sentinel: %w", err)
	}
	return store{
		positions:   posRepo,
		strategies:  posRepo,
		trades:      pg.NewTradeRepo(pool),
		closer:      pg.NewCloser(pool),
		candles:     pg.NewCandleRepo(pool),
		rejections:  pg.NewRejectionRepo(pool),
		advisorRuns: pg.NewAdvisorRunRepo(pool),
		screens:     pg.NewScreenSnapshotRepo(pool),
		// Fresh background context: the setup ctx is cancelled when buildStore
		// returns, but this closure is called later during startup wiring.
		activateCfg: func(rec port.StrategyConfigRecord) error {
			c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return cfgRepo.ActivateExclusive(c, rec)
		},
		backend: "postgres",
		closeFn: pool.Close,
	}, nil
}
