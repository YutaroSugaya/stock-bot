// Command stockbot is the 日本株デイトレ bot entry point (wiring only).
// The fail-safe default is paper mode with the no_trade strategy.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	advisorcli "stockbot/backend/internal/adapter/advisor"
	"stockbot/backend/internal/adapter/apiusage"
	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/notifier"
	"stockbot/backend/internal/app"
	"stockbot/backend/internal/app/freshness"
	"stockbot/backend/internal/app/handler"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
	"stockbot/backend/internal/usecase/command"
	"stockbot/backend/internal/usecase/query"
)

func main() {
	// 🛑 stdout と当日ファイルの両方へ出す。`make start` だけでログが残るようにするため
	// (`| tee` を付け忘れると実機の検証ログが 1 行も残らない)。
	// ファイルを開けなくても起動は止めない。
	w, note, closeLog := logWriter(time.Now())
	defer closeLog()
	logger := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if note != "" {
		logger.Info(note)
	}
	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// runtimeState carries the wiring produced by each start-up phase so run() reads
// as the sequence of phases. Fields are set
// by the phase named in their group and read by later phases only.
type runtimeState struct {
	env            config.Env
	hl             *config.HardLimits
	botCfg         *config.BotConfig
	hours          session.TradingHours
	clk            clock.Clock
	counters       *app.Counters
	emergency      *safety.EmergencyStop
	logger         *slog.Logger
	hotWatch       *app.HotWatch
	usage          *apiusage.Counter
	brokers        brokerSet
	brk            port.LiveBroker
	apiRequests    func() int64
	loopCfg        loopConfig
	stop           context.CancelFunc
	st             store
	pending        *safety.PendingPositions
	engine         *strategy.Engine
	deps           *wiringDeps
	freshThrough   func() string
	csvWatch       *freshness.DailyCSVWatch
	backupW        *freshness.BackupWatch
	loadedCfg      *config.StrategyConfig
	selectorOn     bool
	advisorOn      bool
	advisorTrigger func() error
	bundles        []*app.SymbolBundle
	holders        map[string]*app.ConfigSet
	scanProvider   *app.ScanProvider
	screeners      []strategy.Screener // research が回す screener(`advisor_v2.entries` で絞った集合)
	sel            *app.Selector
	live           *liveTrack
	srv            *http.Server
	onShutdown     func()
}

func run(logger *slog.Logger) error {
	r := &runtimeState{logger: logger}
	if err := r.loadConfig(); err != nil {
		return err
	}
	if err := r.buildBrokers(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	r.stop = stop

	if err := r.openStore(ctx); err != nil {
		return err
	}
	defer r.st.closeFn()
	r.startFreshnessWatches(ctx)
	if err := r.buildBundles(ctx); err != nil {
		return err
	}
	if err := r.startSelector(ctx); err != nil {
		return err
	}
	if err := r.startAdvisor(ctx); err != nil {
		return err
	}
	if err := r.buildTracks(ctx); err != nil {
		return err
	}
	if r.live != nil {
		defer r.live.closeFn()
	}
	r.serveHTTP()
	r.startBackgroundJobs(ctx)
	defer func() {
		if r.onShutdown != nil {
			r.onShutdown()
		}
	}()

	bundles, live, loopCfg, srv := r.bundles, r.live, r.loopCfg, r.srv
	// 🛑 **一括フィードのチャンク段差を記録する**。監視集合は
	// `armed ∪ held` の**トラック和集合**で、実効間隔 = 3秒 × ceil(n/120)。
	// API 回数は増えない(同一銘柄は dedup・paper に口座照会は無い)が、和集合が 120 を
	// 超えると**両トラックの時価が 3秒→6秒**になる。影響は 2 つだけで、どちらも
	// 入口には効かない(入口は前日確定日足だけで決まる):
	// 出口判定の遅延と、**分足記録の密度 = バックテストの入力**(サンプリング周期の
	// 混在に区間が 1 つ増える)。段差が変わった日付は記録する。
	logQuoteChunks(bundles, live, logger)
	runTracks(ctx, bundles, loopCfg, live, logger)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	logger.Info("stockbot stopped")
	return nil
}

// loadConfig reads env / hard_limits / bot_config, validates the fail-close guards and arms the emergency stop.
func (r *runtimeState) loadConfig() error {
	logger := r.logger
	env := config.LoadEnv()

	hl, err := config.LoadHardLimits(env.HardLimitsPath)
	if err != nil {
		return err
	}
	botCfg, err := config.LoadBotConfig(env.BotConfigPath)
	if err != nil {
		return err
	}
	// symbols_file が読めない/空なら起動拒否。静的な symbols: へ黙って縮退すると
	// 「絞ったつもりで絞れていない」ユニバースで走り出す。
	if err := botCfg.ResolveSymbols(); err != nil {
		return err
	}
	warnDroppedUniverse(logger, "research", botCfg.DropSymbolsOutsideWhitelist(hl))
	if err := botCfg.ValidateAgainstHardLimits(hl); err != nil {
		return err
	}
	// Fail loud on an unexecutable 執行区分: otherwise the adapter silently places
	// 現物 while the Position freezes margin.
	if err := botCfg.ValidateBrokerCapabilities(); err != nil {
		return err
	}
	if err := config.RequireLiveGuards(botCfg.Mode); err != nil {
		return err
	}
	// live needs a durable backend: in-memory loses the daily-loss ledger and open
	// positions on restart.
	if err := config.RequireDurableBackend(botCfg.Mode, botCfg.Broker.Kind, env.DatabaseURL); err != nil {
		return err
	}
	hours, err := hl.SessionHours.TradingHours()
	if err != nil {
		return err
	}

	clk := clock.System()
	counters := &app.Counters{}
	panicCounter = counters // so safeRun's recovered panics surface on /api/status
	emergency := safety.NewEmergencyStop(env.EmergencyFlagPath, func(string) { counters.EmergencyTrips.Add(1) }).
		OnTripError(func(reason string, err error) {
			// The trip is live in memory but was NOT persisted, so it would not
			// survive a restart. Surface it as loudly as possible.
			counters.EmergencyTrips.Add(1)
			logger.Error("EMERGENCY TRIP COULD NOT BE PERSISTED — flag file unwritable; process halted in-memory only", "reason", reason, "err", err)
		})
	// Fail closed if the emergency stop cannot engage: refuse to start rather than
	// discover a dead safety switch only when a trip silently no-ops.
	if err := emergency.Preflight(); err != nil {
		return err
	}
	r.env, r.hl, r.botCfg, r.hours, r.clk, r.counters, r.emergency = env, hl, botCfg, hours, clk, counters, emergency
	return nil
}

// buildBrokers builds the broker set, logs the API version and derives the loop intervals.
func (r *runtimeState) buildBrokers() error {
	hl, botCfg, clk, logger := r.hl, r.botCfg, r.clk, r.logger
	// 段階ウォッチ Tier A の共有レジストリ。各 bundle が日次で「前日確定日足が BNF
	// パニックか」を刻み、価格ループの間隔と一括フィードの hot レーンが読む。
	hotWatch := app.NewHotWatch()

	// 立花と同じ集計単位(5:30〜翌3:30・口座単位・CLMID 別)で数える永続カウンタ。
	// プロセスを跨いで残るので make stop/start でも当日の数字がゼロに戻らない。
	usage := apiusage.Open(apiusage.DefaultDir(), "stockbot", clk)

	brokers, err := buildBrokerSet(botCfg, hl, clk, usage)
	if err != nil {
		return err
	}
	// 🛑 「いま叩いている API 版」を起動ログに 1 行残す。v4r9 廃止の
	// 移行中は、版がログから読めないと「本番だけ旧版のまま」が黙って走る。
	logBrokerAPIVersion(logger, brokers)
	// login 応答の予定日告知(版の廃止・書面更新)を新しく知った日だけ Warn する。
	if n, ok := brokerAPIUpdateNotice(brokers); ok {
		warnAPIUpdateNotice(logger, n, apiUpdateNoticePath(), clk())
	}
	brk, apiRequests := brokers.research, brokers.apiRequests
	// ループ間隔は bundle の組み立て(維持率の間引き)より前に要る。broker を作った
	// 直後でないと QuotesBatched が分からないので、ここが最も早い地点。
	loopCfg := defaultLoopConfig(botCfg.Mode, botCfg.Broker.Kind, broker.QuotesBatched(brk))
	r.hotWatch, r.usage, r.brokers, r.brk, r.apiRequests, r.loopCfg = hotWatch, usage, brokers, brk, apiRequests, loopCfg
	return nil
}

// openStore opens the ledger, restores the paper book and assembles the per-symbol wiring deps.
func (r *runtimeState) openStore(ctx context.Context) error {
	env, hl, botCfg, hours, clk, counters, emergency, logger, brk := r.env, r.hl, r.botCfg, r.hours, r.clk, r.counters, r.emergency, r.logger, r.brk
	st, err := buildStore(ctx, env, logger)
	if err != nil {
		return err
	}

	// 紙の建玉帳をプロセス跨ぎで復元する(paper / paper_live_feed のみ)。
	if err := adoptPaperBook(ctx, brk, st.positions, logger); err != nil {
		return err
	}

	pending := safety.NewPendingPositions()

	var signalSeq counter
	engine := buildStrategyEngine(func() string { return "sig-" + signalSeq.next() })

	deps := &wiringDeps{
		broker: brk, posRepo: st.positions, tradeRepo: st.trades, closer: st.closer, candles: st.candles,
		rejections: st.rejections, pending: pending, emergency: emergency, engine: engine, hours: hours,
		clock: clk, botCfg: botCfg, hardLimits: hl, counters: counters, logger: logger,
	}
	r.st, r.pending, r.engine, r.deps = st, pending, engine, deps
	return nil
}

// startFreshnessWatches seeds daily candles and starts the read-only CSV / DB-dump freshness monitors.
func (r *runtimeState) startFreshnessWatches(ctx context.Context) {
	env, botCfg, hours, clk, logger, st := r.env, r.botCfg, r.hours, r.clk, r.logger, r.st
	if dir := os.Getenv("STOCKBOT_DAILY_CANDLES_DIR"); dir != "" {
		// 🛑 ユニバースだけでは足りない。ユニバースから外れた保有銘柄の日足が凍結し、
		// 3 営業日で day-horizon 評価が止まる(candle_coverage.go)。
		if n, err := seedDailyCandles(ctx, st.candles, r.candleSymbols(ctx), dir); err != nil {
			logger.Warn("daily candle seed failed", "dir", dir, "err", err)
		} else {
			logger.Info("seeded daily candles", "dir", dir, "candles", n)
		}
	}

	// 「今この瞬間に存在しうる最新の日足」(venue TZ の "2006-01-02")。空文字 =
	// 休場カレンダー失効で判定不能(取引自体は session 側が fail-close で止める)。
	freshThrough := func() string {
		d, ok := hours.LastClosedTradingDay(clk())
		if !ok {
			return ""
		}
		return d
	}

	// 日足 CSV の鮮度監視(読むだけ)。基準は freshThrough(DB 用)ではなく
	// csvFreshThrough — CSV は常に1営業日うしろにいるのが正常。
	// プールから外した銘柄(上場廃止など)の CSV は鮮度の警告に数えない(retired/ へ移すまで鳴り続けた)。
	csvWatch := &freshness.DailyCSVWatch{InPool: r.hl.AllowsSymbol}
	if dir := os.Getenv("STOCKBOT_DAILY_CANDLES_DIR"); dir != "" {
		csvThrough := func() string { return csvFreshThrough(hours, clk()) }
		csvWatch.Refresh(dir, botCfg.Symbols, csvThrough(), logger)
		go runTicker(ctx, time.Hour, logger, "universe", "daily-csv-freshness", func() {
			csvWatch.Refresh(dir, botCfg.Symbols, csvThrough(), logger)
		})
	}

	// DB dump の鮮度監視(読むだけ)。
	backupW := &freshness.BackupWatch{}
	if dir := freshness.DefaultBackupDir(); dir != "" {
		// 🚨 **このプロセスが実際に使っている DB 名**を渡す。渡さないと「dump が
		// 1 本もある DB」しか監視されず、新しく切った DB が「dump されない ×
		// 監視されない」に同時に落ちる。env から導くので、DB を切り替えても一覧の更新は要らない。
		expectedDBs := usedDatabaseNames(env.DatabaseURL, os.Getenv("STOCKBOT_LIVE_DATABASE_URL"))
		backupW.RefreshFor(dir, expectedDBs, clk(), logger)
		go runTicker(ctx, time.Hour, logger, "db-backup", "db-backup-freshness", func() {
			backupW.RefreshFor(dir, expectedDBs, clk(), logger)
		})
	}
	r.freshThrough, r.csvWatch, r.backupW = freshThrough, csvWatch, backupW
}

// buildBundles resolves the watched universe and builds one SymbolBundle (+ config holder) per symbol.
func (r *runtimeState) buildBundles(ctx context.Context) error {
	env, hl, botCfg, logger, hotWatch, loopCfg, st, engine, deps := r.env, r.hl, r.botCfg, r.logger, r.hotWatch, r.loopCfg, r.st, r.engine, r.deps
	loadedCfg := loadActiveConfigOrNil(env.StrategyConfigPath, hl, logger)

	selectorOn := botCfg.Selector.Enabled
	advisorOn := botCfg.Advisor.Enabled
	// dashboard の「今すぐ判断」ボタン用。advisor ループ構築時に StartManual を刺す。
	var advisorTrigger func() error
	// Both enabled would silently skip the Selector's live-allowlist validation
	// below — fail loud instead of picking a winner.
	if advisorOn && selectorOn {
		return fmt.Errorf("selector.enabled and advisor.enabled are mutually exclusive (both arm the same holders); disable one")
	}
	if advisorOn && botCfg.Mode != config.ModePaper {
		return fmt.Errorf("advisor.enabled requires bot mode paper_config (advisor generates paper configs only); got %q", botCfg.Mode)
	}
	// 監視対象 = 日次ユニバース ∪ 建玉のある銘柄。合流ぶんは **no_trade 固定**で、
	// 監視して閉じるだけ・新規建玉はしない。
	watchSyms, adoptedSyms, err := watchedSymbols(ctx, st.positions, botCfg.Symbols)
	if err != nil {
		return err
	}
	adopted := make(map[string]bool, len(adoptedSyms))
	for _, s := range adoptedSyms {
		adopted[s] = true
	}
	if len(adoptedSyms) > 0 {
		// 決済されれば次の起動で消える。居座るなら決済が詰まっているサイン。
		logger.Warn("ユニバース外の建玉を監視対象に合流(no_trade で決済のみ)",
			"symbols", adoptedSyms, "universe", len(botCfg.Symbols))
		for _, s := range adoptedSyms {
			if !hl.AllowsSymbol(s) {
				// 閉じることは許す(持っているものを決済できない方が危険)。新規建玉は
				// no_trade + whitelist の二重で不可。
				logger.Warn("allowed_symbols から外れた銘柄の建玉が残っている(決済のみ許可)", "symbol", s)
			}
		}
	}
	bundles := make([]*app.SymbolBundle, 0, len(watchSyms))
	holders := make(map[string]*app.ConfigSet, len(watchSyms))
	for _, sym := range watchSyms {
		active := defaultNoTradeConfig(sym, botCfg.Mode)
		// 合流した(= ユニバース外の)銘柄には決して config を当てない。
		if !selectorOn && !advisorOn && !adopted[sym] {
			if cand := configForSymbol(loadedCfg, sym); cand != nil {
				if err := cand.ValidateAgainstHardLimits(hl); err != nil {
					logger.Warn("active config rejected for symbol; using no_trade", "symbol", sym, "err", err)
				} else {
					active = cand
				}
			}
		}
		// Fail loud on an unregistered strategy: the engine would otherwise degrade
		// to NO_TRADE at runtime and the misconfiguration stays invisible.
		if !engine.Has(active.StrategyName) {
			return fmt.Errorf("active config for %q names unregistered strategy %q (registered: %s); register it in cmd/stockbot or fix strategy_name", sym, active.StrategyName, strings.Join(engine.Names(), ", "))
		}
		// live 戦略 allowlist の runtime 強制(昇格は人間 commit のみ)。
		if botCfg.Mode == config.ModeLive && !hl.AllowsLiveStrategy(active.StrategyName) {
			return fmt.Errorf("live_config refuses strategy %q for %q: not in hard_limits live_allowed_strategies (promotion to live is a human commit)", active.StrategyName, sym)
		}
		// Postgres FK: the position's config_id must exist before any insert.
		// RawYAML = 全文監査("その時どの config で動いていたか" を後から読める)。
		if st.activateCfg != nil {
			if err := st.activateCfg(activeConfigRecord(active)); err != nil {
				return fmt.Errorf("persist active config %q: %w", active.ConfigID, err)
			}
		}
		b := buildSymbolBundle(deps, sym, active)
		b.HotWatch = hotWatch
		// 維持率の口座照会を間引くのは **実 broker のときだけ**。paper の
		// GetAccountMargin はプロセス内の変数なので、間引く動機(wire コスト)が
		// 無い上に、紙の forward が本番と違う頻度でブレーカーを見ることになる。
		if botCfg.Broker.Kind == config.BrokerTachibana {
			b.MarginPollInterval = loopCfg.marginHeldInterval
		}
		if selectorOn || advisorOn {
			b.MonitorNarrow = true // only armed/held symbols poll the broker
		}
		bundles = append(bundles, b)
		if !adopted[sym] {
			// 合流ぶんは holders に載せない = advisor / Selector が arm する経路が
			// 型として存在しない。
			holders[sym] = b.Configs
		}
	}
	r.loadedCfg, r.selectorOn, r.advisorOn, r.advisorTrigger, r.bundles, r.holders = loadedCfg, selectorOn, advisorOn, advisorTrigger, bundles, holders
	return nil
}

// startSelector wires the deterministic selector (live path: one config per symbol).
func (r *runtimeState) startSelector(ctx context.Context) error {
	hl, botCfg, clk, logger, st, engine, deps, loadedCfg, selectorOn, advisorOn, holders := r.hl, r.botCfg, r.clk, r.logger, r.st, r.engine, r.deps, r.loadedCfg, r.selectorOn, r.advisorOn, r.holders
	var sel *app.Selector
	// research が回す screener は `advisor_v2.entries` で入口単位に絞った集合。
	// dashboard のランキングも同じ集合 — 止めた入口を画面にだけ出すと「発火しているのに建たない」に見える。
	screeners, err := researchScreeners(botCfg)
	if err != nil {
		return err
	}
	r.screeners = screeners
	// The dashboard ranking is served whether or not auto-arming is on.
	scanProvider := app.NewScanProvider(st.candles, botCfg.Symbols, screeners, 30*time.Second, clk)
	if selectorOn && !advisorOn {
		if loadedCfg == nil || !strategy.LiveArmable(loadedCfg.StrategyName) {
			return fmt.Errorf("selector.enabled requires STOCKBOT_STRATEGY_CONFIG to point at an armable universe template (bnf_reversion / bnf_intraday_reversion); got %q", strategyNameOrNone(loadedCfg))
		}
		if !engine.Has(loadedCfg.StrategyName) {
			return fmt.Errorf("selector template strategy %q is not registered in the engine", loadedCfg.StrategyName)
		}
		if botCfg.Mode == config.ModeLive && !hl.AllowsLiveStrategy(loadedCfg.StrategyName) {
			return fmt.Errorf("live_config refuses selector template strategy %q: not in hard_limits live_allowed_strategies (edge-proof promotion is a human commit)", loadedCfg.StrategyName)
		}
		armCfg := func(sym string) *config.StrategyConfig {
			c := configForSymbol(loadedCfg, sym)
			if c == nil {
				return nil
			}
			if err := c.ValidateAgainstHardLimits(hl); err != nil {
				logger.Warn("selector: armed config rejected by hard limits", "symbol", sym, "err", err)
				return nil
			}
			return c
		}
		noTradeCfg := func(sym string) *config.StrategyConfig { return defaultNoTradeConfig(sym, botCfg.Mode) }
		persist := func(c *config.StrategyConfig) error {
			if st.activateCfg == nil {
				return nil
			}
			return st.activateCfg(activeConfigRecord(c))
		}
		if err := app.ValidateSelectorScreeners(strategy.LiveArmScreeners(loadedCfg.StrategyName)); err != nil {
			return err
		}
		// 🛑 Selector は **1銘柄1 config のまま**(live の経路。緩めるのは paper だけ)。
		// ConfigSet の base holder を渡す = arm 集合には触れない。
		sel = app.NewSelector(st.candles, st.positions, baseHolders(holders), strategy.LiveArmScreeners(loadedCfg.StrategyName),
			botCfg.Risk.AccountMaxOpenPositions, botCfg.Selector.MaxPositionNotionalJPY, armCfg, noTradeCfg, persist, logger)
		// research はレバ上限を持たない(ratio 0 = 無効)。それでも**配線はする** —
		// live 側だけに挿すと、片方の経路にしか無い規律が「たまたまそう書いてある」に
		// 退化する。値が 0 のときは口座照会も一度も走らない。
		sel.WithLeverageHeadroom(deps.accountMargin(), botCfg.Risk.MaxGrossNotionalRatio)
		sel.Tick(ctx)
		interval := time.Duration(botCfg.Selector.RearmIntervalMin) * time.Minute
		if interval <= 0 {
			interval = time.Hour
		}
		go runTicker(ctx, interval, logger, "universe", "selector", func() { sel.Tick(ctx) })
		logger.Info("universe selector enabled", "symbols", len(holders), "template", loadedCfg.StrategyName,
			"rearm_interval", interval, "account_max", botCfg.Risk.AccountMaxOpenPositions)
	}
	r.sel, r.scanProvider = sel, scanProvider
	return nil
}

// startAdvisor wires the paper-only advisor loop (deterministic arm or LLM). Never on the order path.
func (r *runtimeState) startAdvisor(ctx context.Context) error {
	hl, botCfg, hours, clk, logger, st, engine, advisorOn, advisorTrigger, holders := r.hl, r.botCfg, r.hours, r.clk, r.logger, r.st, r.engine, r.advisorOn, r.advisorTrigger, r.holders
	// AI advisor loop (paper-only; replaces the Selector's auto-arm). Each tick
	// ranks the universe deterministically, then the LLM generates a config for
	// the top candidate. **The LLM is never on the order path.**
	if advisorOn {
		deterministic := !botCfg.Advisor.UsesLLM()
		var adv port.Advisor
		if !deterministic {
			if botCfg.Advisor.PromptPath == "" {
				return fmt.Errorf("advisor.enabled requires advisor.prompt_path (e.g. prompts/generate_strategy_config.md)")
			}
			cliPath := botCfg.Advisor.CLIPath
			if cliPath == "" {
				cliPath = "claude"
			}
			cli := advisorcli.New(cliPath, botCfg.Advisor.PromptPath, botCfg.Advisor.WorkingDir, botCfg.Advisor.TimeoutSeconds)
			cli.Logger = logger
			if cli.OutputDir = botCfg.Advisor.OutputDir; cli.OutputDir == "" {
				cli.OutputDir = advisorcli.DefaultOutputDir() // 各 run の生 stdout の監査用アーカイブ(~/.stockbot 配下)
			}
			adv = cli
		}
		arm := func(c *config.StrategyConfig) error {
			if !engine.Has(c.StrategyName) {
				return fmt.Errorf("unregistered strategy %q", c.StrategyName)
			}
			cs, ok := holders[c.Symbol]
			if !ok {
				return fmt.Errorf("no holder for symbol %q (not in bot_config symbols)", c.Symbol)
			}
			if st.activateCfg != nil {
				rec := activeConfigRecord(c)
				rec.AdvisorRunID = c.AdvisorRunID // advisor 由来のときだけ刻む
				if err := st.activateCfg(rec); err != nil {
					return fmt.Errorf("persist advisor config %q: %w", c.ConfigID, err)
				}
			}
			// (銘柄, 戦略) 単位で載せる。同一銘柄の別戦略を**上書きしない**。
			cs.Arm(c)
			return nil
		}
		// 決定論 arm。`AdvisorCycle.Run`(LLM)を置き換える 1 点で、枠の配り方・
		// ランキング・risk gate・発注は従来どおり。Promoter は残す —
		// 「メニュー外」「live_config 混入」「holding_mode 空」「margin_oneday」は
		// テンプレートでもコードのバグとして起こりうる。
		var buildConfig func(string, config.StrategyName, []market.Candle) (*config.StrategyConfig, error)
		if deterministic {
			tmpl := &command.ArmTemplate{
				HardLimits:  hl,
				Mode:        botCfg.Mode,
				ExecKindFor: botCfg.ExecKindFor,
			}
			buildConfig = func(sym string, slot config.StrategyName, daily []market.Candle) (*config.StrategyConfig, error) {
				if len(daily) == 0 {
					return nil, fmt.Errorf("%s: 日足が無く直近終値を取れない(fail-close)", sym)
				}
				cfg, err := tmpl.Build(sym, slot, daily[len(daily)-1].Close, clk())
				if err != nil {
					return nil, err
				}
				p := &command.Promoter{
					HardLimits: hl, ExpectedSymbol: sym,
					Menu: command.AdvisorCandidateStrategies, ExpectedStrategy: slot,
				}
				return p.PromoteConfig(cfg)
			}
		}
		loop := &app.AdvisorLoop{
			Symbols:     botCfg.Symbols,
			Candles:     st.candles,
			Screeners:   r.screeners, // `advisor_v2.entries` で絞った集合(startSelector が解決済み)
			Advisor:     adv,
			HardLimits:  hl,
			Menu:        command.AdvisorCandidateStrategies,
			Hours:       hours,
			Clock:       clk,
			Arm:         arm,
			BuildConfig: buildConfig,
			Screens:     st.screens,
			// 🚨 出せない売りに枠を配らない。発注前ゲートは
			// arm の後なので、これが無いと建玉にならない売り候補が per_strategy_n を
			// 占有し、同じ戦略の買い候補が arm されなくなる。
			// 🛑 **一覧が未 commit なら nil**(= 間引かない)。枠の前で落とすと
			// `loanable_list_missing` が signal_rejections に残らず、「売り標本が
			// ゼロなのは一覧が無いから」と後から判らなくなる。
			ShortAllowed: app.ShortAllowedOrNil(hl),
			// 建玉判定は **(銘柄, 戦略)**。銘柄で見ると、片方のアームが建った瞬間に
			// 同じ銘柄のもう片方が永久に arm されない(兄弟アームのペアが 0 本になる原因)。
			// 🛑 **戦略不明の建玉(external / 旧建玉)は全戦略を建玉中とみなす** —
			// ナンピン禁止ゲートが同じ倒し方をするので、arm 側だけ緩めると「arm したのに
			// 毎ティック reject される」銘柄が生まれる。
			IsHeld: heldByStrategy(st.positions),
			Disarm: func(sym string, name config.StrategyName) error {
				cs, ok := holders[sym]
				if !ok {
					return fmt.Errorf("no holder for %q", sym)
				}
				cs.Disarm(name)
				return nil
			},
			TopN:          botCfg.Advisor.TopN,
			PerStrategyN:  botCfg.Advisor.PerStrategyN,
			MaxConcurrent: botCfg.Advisor.MaxConcurrent,
			// 資金キャパシティ・フィルタは selector 経路と**同じ knob** を読む。
			MaxPositionNotionalJPY: botCfg.Selector.MaxPositionNotionalJPY,
			Notifier:               notifier.NewStdout(),
			Runs:                   st.advisorRuns,
			Logger:                 logger,
		}
		// The ticker drives the CHEAP deterministic scan; the LLM only runs on a new
		// trigger / session open / heartbeat, so a fresh panic is picked up within
		// one scan instead of up to an hour.
		if loop.Heartbeat = time.Duration(botCfg.Advisor.IntervalMin) * time.Minute; loop.Heartbeat <= 0 {
			loop.Heartbeat = time.Hour
		}
		// heartbeat を短くしても screen_snapshots の書込量が増えないよう周期を分離。
		loop.SnapshotInterval = time.Duration(botCfg.Advisor.SnapshotIntervalMin) * time.Minute
		loop.PreOpenLead = time.Duration(botCfg.Advisor.PreOpenMin) * time.Minute
		advisorTrigger = func() error { return loop.StartManual(ctx) }
		scan := time.Duration(botCfg.Advisor.ScanIntervalSec) * time.Second
		if scan <= 0 {
			scan = time.Minute
		}
		// The first scan runs ASYNCHRONOUSLY: it may invoke `claude` (up to
		// timeout_seconds), and blocking here would delay ListenAndServe — leaving
		// /api/emergency-* unreachable on a bot that is already arming.
		go func() {
			loop.Tick(ctx)
			runTicker(ctx, scan, logger, "universe", "advisor", func() { loop.Tick(ctx) })
		}()
		armMode := "llm"
		if deterministic {
			armMode = "deterministic"
		}
		logger.Info("advisor loop enabled (paper)", "arm", armMode, "symbols", len(holders),
			"scan_interval", scan, "heartbeat", loop.Heartbeat,
			"snapshot_interval", loop.SnapshotInterval,
			"top_n", botCfg.Advisor.TopN, "per_strategy_n", botCfg.Advisor.PerStrategyN,
			"max_concurrent", botCfg.Advisor.MaxConcurrent,
			"prompt", botCfg.Advisor.PromptPath)
	}
	r.advisorTrigger = advisorTrigger
	return nil
}

// buildTracks starts the quote-chunk watch and the optional live track.
func (r *runtimeState) buildTracks(ctx context.Context) error {
	env, hl, botCfg, hours, clk, logger, hotWatch, brokers, brk, loopCfg, st, engine := r.env, r.hl, r.botCfg, r.hours, r.clk, r.logger, r.hotWatch, r.brokers, r.brk, r.loopCfg, r.st, r.engine
	// 監視銘柄の積み上がりは **実行時**の変化なので、構成の上限しか見ないテストでは
	// 捕まらない(建玉は決済まで積み上がる)。120 を超えた日にログで気づけるようにする。
	// 通信量は BatchQuoteFeed が間隔を伸ばして一定に保つが、**1分足の解像度が落ちる**
	// ので、検定の入力が変わったことを後から説明できる記録が要る。
	go runTicker(ctx, 15*time.Minute, logger, "universe", "quote-chunk-watch", func() {
		warnOnQuoteChunkGrowth(broker.QuoteChunks(brk), loopCfg, logger)
	})

	// hybrid: 実弾トラックの追加起動(env 未設定なら nil = 現行と完全に同一)。
	// 🛑 research 側の組み立ては 1 行も変えない。live は**足すだけ**。
	live, err := buildLiveTrack(ctx, config.LoadLiveTrackEnv(), env, botCfg, liveTrackDeps{
		hardLimits: hl, hours: hours, engine: engine, clock: clk,
		// 日足は research 側の repo を共有する(市場データであって台帳ではない)。
		// live 自身の空の DB を見せると selector が永久に何も選べない。
		candles: st.candles,
		tb:      brokers.tb, feed: brokers.feed, hotWatch: hotWatch, logger: logger,
	})
	if err != nil {
		return err
	}

	// 🛑 ここで closeFn を defer しない。**buildTracks が return した瞬間**に live の
	// プールが閉じ、以降の台帳操作が全て `closed pool` で失敗する。解放は run が持つ
	// (`pool_ownership_test.go` が固定)。
	r.live = live
	return nil
}

// serveHTTP assembles the dashboard queries and starts the control-plane server (the kill switch).
func (r *runtimeState) serveHTTP() {
	env, hl, botCfg, hours, clk, counters, emergency, logger, usage, brk, apiRequests, stop, st, csvWatch, backupW, selectorOn, advisorTrigger, bundles, scanProvider, sel, live := r.env, r.hl, r.botCfg, r.hours, r.clk, r.counters, r.emergency, r.logger, r.usage, r.brk, r.apiRequests, r.stop, r.st, r.csvWatch, r.backupW, r.selectorOn, r.advisorTrigger, r.bundles, r.scanProvider, r.sel, r.live
	statusQ := query.NewGetBotStatus(st.positions, emergency, string(botCfg.Mode), string(botCfg.Broker.Kind))
	if st.strategies != nil {
		statusQ = statusQ.WithStrategyResolver(st.strategies)
	}
	listQ := query.NewListOpenPositions(st.positions)
	extendCmd := command.NewExtendMaxHold(st.positions, emergency)
	// ユニバース外に落ちた銘柄の建玉は bundle が無く nil,nil が返り、closeOne の
	// GetTicker に落ちる。
	bundleBySym := make(map[string]*app.SymbolBundle, len(bundles))
	for _, b := range bundles {
		bundleBySym[b.Symbol] = b
	}
	closeCmd := command.NewCloseAllOpen(brk, st.positions, st.closer,
		func(symbol string) (*market.MarketSummary, *market.MarketSummary) {
			b, ok := bundleBySym[symbol]
			if !ok {
				return nil, nil
			}
			return b.LastSummary(), b.LastIndicative()
		}, clk,
		// 🛑 紙のトラックは警報なし(紙の close 失敗で測定用の bot を止めない)。
		// **ただし main.go は「必ず紙」ではない** — hybrid env(STOCKBOT_LIVE_BOT_CONFIG)
		// が無い単一トラック構成では config/hybrid.go の ValidateHybrid が early return
		// するので research 側 mode の検査が走らず、mode: live_config + tachibana で
		// このトラック自身が実弾になる。そこで nil を焼き込むと、直したはずの穴を
		// 1 箇所に植え直すことになる。
		func() command.EmergencyController {
			if botCfg.Mode == config.ModeLive {
				return emergency
			}
			return nil
		}()).
		WithCarry(hl.CarryCalc(hours)).
		WithLogger(logger.Info)
	activeBySym := func() map[string]string {
		m := make(map[string]string, len(bundles))
		for _, b := range bundles {
			m[b.Symbol] = strings.Join(b.Configs.ConfigIDs(), ",")
		}
		return m
	}
	// 寄り前の go/no-go(paper は bnf 家族だけ判定する)。表示だけ。
	gonogoPanel := newGoNoGoPanel("research", isPaperGoNoGoFamily, clk, logger)
	dashExtra := func() map[string]any {
		sums := quoteViews(bundles)
		ranking := app.RankingView(context.Background(), scanProvider, bundles, st.positions, sel)
		// armed_symbol は Selector の契約(armed 集合をソートした先頭)そのものなので
		// インライン展開しない — backend/cmd/** はテスト存在チェックの免除対象で、
		// 展開すると同じ規則の 2 か所のうちこちらだけがテスト無しになる。
		armed := ""
		var armedSyms []string
		// -1 = 未計測。research は selector を切ってあり(advisor 経路が arm する)、
		// 金額上限は advisor 側の同じ knob で効く — 内数は selector がある時だけ出す。
		affordable, maxNotional := -1, botCfg.Selector.MaxPositionNotionalJPY
		if sel != nil {
			armedSyms = sel.ArmedSymbols()
			armed = sel.ArmedSymbol()
			affordable = sel.AffordableCount()
			maxNotional = sel.MaxNotionalJPY()
		}
		// 監視銘柄の予算。**枠が効いているかを画面から読めるようにする** —
		// これは標本を切る censoring なので、掛かっていることが黙って消えてはいけない。
		// 本数と銘柄数を 1 クエリで取る(dashboard は 1 秒に何度も叩かれる経路ではない)。
		counts, _ := st.positions.CountOpenAcross(context.Background(), nil)
		openN := counts.Positions
		return map[string]any{
			"counters":  counters.Snapshot(),
			"summaries": sums,
			"daily_csv": csvWatch.Get(),
			"db_backup": backupW.Get(),
			// 立花への wire 呼び出し回数(この起動からの累計)。削減の申告を見積りでは
			// なく実測で確かめる観測点で、チャンク分割・login・再送も全部入る。
			"api_requests": apiRequests(),
			// 立花と同じ集計単位(5:30〜翌3:30・全プロセス合算・CLMID 別)の実数。
			// 再起動でゼロに戻らないので「今日いくつ叩いたか」に答えられる唯一の数字。
			"api_usage": apiUsageView(usage.Snapshot(), broker.QuoteChunks(brk)),
			"selector": map[string]any{
				"enabled":       selectorOn,
				"armed_symbol":  armed,
				"armed_symbols": armedSyms,
				"account_open":  openN,
				"account_max":   botCfg.Risk.AccountMaxOpenPositions,
				// 🚨 **本数の枠とは別物**。監視集合(= 時価の 120 銘柄/リクエスト枠)に
				// 効くのは銘柄数だけで、同じ銘柄に 12 アーム乗っても 1 銘柄。
				"account_open_symbols":  counts.Symbols,
				"account_max_symbols":   botCfg.Risk.AccountMaxOpenSymbols,
				"per_entry_max_symbols": botCfg.Risk.PerEntryMaxOpenSymbols,
				"universe":              len(botCfg.Symbols),
				"affordable":            affordable,
				"max_notional_jpy":      maxNotional,
				"ranking":               ranking,
			},
			"gonogo": gonogoPanel(ranking),
		}
	}
	perfTable := loadPerfCycles(r.logger)
	h := handler.New(statusQ, listQ, emergency, extendCmd, activeBySym, dashExtra).
		WithAuth(env.APIToken, !isLoopbackAddr(env.HTTPAddr)).
		WithAdvisorRuns(query.NewListAdvisorRuns(st.advisorRuns)).
		WithPerformance(query.NewBuildForwardReport(st.trades, perfReportOptions(st.strategies)...)).
		WithPerformanceCycles(perfTable.Current, r.researchPerfCycles(perfTable)).
		// mode は string で渡す — handler は config を知らない。
		WithManualClose(closeCmd, string(botCfg.Mode)).
		WithGoNoGoRun(gonogoRun)
	if advisorTrigger != nil {
		h.WithAdvisorTrigger(advisorTrigger)
	}
	// hybrid: live トラックの読み書き口を /api/live/* に足す。既存ルートは research の
	// まま不変で、束も別インスタンス(損益を混ぜない)。
	if live != nil {
		h.WithLiveTrack(live.httpViews(hl, hours, clk, st.candles, live.broker, logger))
	}
	// Timeouts are not optional: this server IS the kill switch and half-open
	// connections holding fds indefinitely would cost us the control plane.
	// WriteTimeout stays unset — the dashboard aggregates can be slow, and a
	// truncated response would be worse than a slow one.
	srv := &http.Server{
		Addr:              env.HTTPAddr,
		Handler:           h.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("http listening", "addr", env.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Trading on with a dead control plane (= no kill switch) is worse than
			// stopping: cancel the root ctx so the loops shut down too.
			logger.Error("http server died — shutting down the bot (control plane / kill switch unavailable)", "err", err)
			stop()
		}
	}()
	r.srv = srv
}

// startBackgroundJobs starts daily-candle refresh, the minute-bar recorder, token refresh, after-close jobs and freshness warnings.
func (r *runtimeState) startBackgroundJobs(ctx context.Context) {
	botCfg, hours, clk, emergency, logger, brk, st, freshThrough, advisorOn, bundles, live := r.botCfg, r.hours, r.clk, r.emergency, r.logger, r.brk, r.st, r.freshThrough, r.advisorOn, r.bundles, r.live
	var onShutdown func()
	// Daily-candle refresh. Gate on the broker's capability (not on mode): paper
	// serves no klines, while tachibana / paper_live_feed both serve the real 日足。
	// 定期リフレッシュは立花の推奨時間帯の中だけで動かす(history_window.go)。
	if botCfg.Broker.Kind.ServesKlines() {
		// 起動時だけは窓の外でも引く: 日足が欠けたまま day-horizon 戦略を回す方が
		// 害が大きい(古いバーで乖離を誤判定する)。鮮度ゲートがあるので、既に
		// 最新を持っていればここは 0 リクエストで終わる。
		if !inHistoryFetchWindow(clk()) {
			logger.Warn("起動時の日足取得が立花の推奨時間帯の外です(推奨: 18:00〜翌3:30 / 5:30〜8:00)。"+
				"AM8:00 より前の起動を推奨", "now", clk().In(clock.JST).Format("15:04"))
		}
		// ここは buildTracks の後なので live の建玉も合流する。起動時の
		// CSV seed(research だけ)が拾えなかったぶんはここで埋まる。
		if n := refreshDailyCandles(ctx, st.candles, brk, r.candleSymbols(ctx), freshThrough(), logger); n > 0 {
			logger.Info("daily candles refreshed at startup", "symbols", n)
		}
		go runTicker(ctx, time.Hour, logger, "universe", "daily-refresh", func() {
			if !inHistoryFetchWindow(clk()) {
				return // 立花の推奨時間帯の外では履歴を引かない
			}
			refreshDailyCandles(ctx, st.candles, brk, r.candleSymbols(ctx), freshThrough(), logger)
		})
	} else if dir := os.Getenv("STOCKBOT_DAILY_CANDLES_DIR"); dir != "" && advisorOn {
		// PAPER + advisor: the paper broker serves no klines, so without re-reading
		// the CSV dir the candle set stays frozen at the startup seed, RankCandidates
		// returns an identical result every scan, and the advisor's rising-edge
		// "new trigger" can NEVER fire (silently degrading to heartbeat-only).
		go runTicker(ctx, 10*time.Minute, logger, "universe", "daily-reseed", func() {
			if n, err := seedDailyCandles(ctx, st.candles, r.candleSymbols(ctx), dir); err != nil {
				logger.Warn("daily candle re-seed failed", "dir", dir, "err", err)
			} else if n > 0 {
				logger.Info("daily candles re-seeded", "dir", dir, "candles", n)
			}
		})
	}

	// Minute-bar recorder (opt-in): 立花 serves NO minute-bar history, so the
	// bnf_intraday edge test can only use bars WE persist from the live/demo feed.
	// Pointless on paper (synthetic prices).
	if dir := os.Getenv("STOCKBOT_RECORD_CANDLES_DIR"); dir != "" {
		rec := app.NewCandleRecorder(dir)
		go runTicker(ctx, time.Minute, logger, "universe", "candle-recorder", func() {
			now := clk()
			for _, b := range bundles {
				for _, iv := range []time.Duration{time.Minute, 5 * time.Minute} {
					if _, err := rec.Record(b.Symbol, iv, b.Agg.Candles(iv), now); err != nil {
						logger.Warn("candle recorder write failed", "symbol", b.Symbol, "err", err)
					}
				}
			}
		})
		logger.Info("minute-bar recorder enabled", "dir", dir)
	}

	// 立花 は API 閉局 03:30 で毎日セッションを落とすので、閉局をまたいだら 1 回張り直す
	// (集計窓に 1 回だけ。立花はログインを 1 日 1 回に留めるよう求めているので毎時の張り直しはやめた)。
	// paper_live_feed も対象: 執行は紙でも **フィードは実 API** なので、維持しないと
	// 日次切断後 GetTicker が落ち続け、紙の forward が静かに止まる。
	switch botCfg.Broker.Kind {
	case config.BrokerTachibana, config.BrokerPaperLiveFeed:
		window := maintenanceWindowFor(botCfg.Broker.Kind)
		if s, ok := r.brokers.tb.(interface{ LastLoginAt() time.Time }); ok {
			go runDailyLoginLoop(ctx, brk, clk, s.LastLoginAt, window, func(reason string) { _ = emergency.Trip(reason, clk()) }, logger)
		} else {
			logger.Error("立花のセッションが最終ログイン時刻を持たない — 日次の張り直しを回さない(p_errno=2 の張り直しだけになる)")
		}
	}

	if botCfg.Broker.Kind == config.BrokerTachibana && botCfg.Mode == config.ModeLive &&
		getenv("STOCKBOT_TACHIBANA_ENV", "demo") != "production" {
		logger.Warn("RUNNING live_config AGAINST THE TACHIBANA DEMO HOST — orders are simulated, NOT real money (set STOCKBOT_TACHIBANA_ENV=production for live)")
	}

	// 引け後の記録ジョブ(launchd に足さず bot の中で回す — 人間の運用は start/stop だけ)。
	// **取引経路とは何も共有しない**: 別 goroutine・panic recover・タイムアウト付きで、
	// 失敗しても bot は落ちない。停止時にも 1 回走らせて取りこぼしを拾う。
	afterClose := newAfterCloseJobs(botCfg, hours, clk, logger)
	if afterClose != nil {
		go runTicker(ctx, time.Minute, logger, "universe", "after-close", func() { afterClose.Tick(ctx) })
		onShutdown = func() {
			// 停止時の 1 回。引け直後(15:40 より前)に停止されると当日の段1 が
			// 書かれないため(同日は 1 回だけなので二重実行は無害)。
			runCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			afterClose.RunNow(runCtx)
		}
	}

	logUniverseProvenance(botCfg, clk, logger)
	// 起動時の1回だけでは「起動しっぱなしで日を跨ぐ」を捕まえられない(起動した日は
	// 正常だった)。掴んだ mtime を固定して定期的に鳴らす — 詳細は universe_freshness.go。
	if loadedAt := universeModTime(botCfg.SymbolsFile); !loadedAt.IsZero() {
		go runTicker(ctx, time.Hour, logger, "universe", "universe-freshness", func() {
			warnStaleUniverse(loadedAt, clk(), hours, botCfg.SymbolsFile, logger)
		})
	}
	logStarted(logger, botCfg.Mode, botCfg.Broker.Kind, botCfg.Symbols, len(bundles),
		botCfg.Advisor.Entries, len(r.screeners), live != nil)
	// 🛑 **上書きせず連結する**。serveHTTP が先に過去の期間の DB を閉じる処理を積んでいる
	// (perf_cycles.go)。上書きすると読み取り専用プールが shutdown で閉じられない。
	prev := r.onShutdown
	r.onShutdown = func() {
		if onShutdown != nil {
			onShutdown()
		}
		if prev != nil {
			prev()
		}
	}
}

// buildStrategyEngine は戦略カタログのメニュー(`strategy.MenuStrategies`)を engine に載せる。
// **登録漏れは致命的**(menu と screener にだけ載ると枠を消費した末に Arm で unregistered として
// 弾かれ、標本ゼロのまま気づけない)ので、ここに名前を写さない。
// 登録だけでは何も建たない — active config が指名した戦略だけが動き、live の allowlist は別に効く。
// 棄却済み・v1 の戦略は載せない(実装はカタログに残り、cmd/backtest で再現できる)。
func buildStrategyEngine(signalID strategy.SignalIDFn) *strategy.Engine {
	return strategy.NewEngine(signalID, strategy.MenuStrategies()...)
}

// loadActiveConfigOrNil returns nil (and logs) when the config is absent or
// invalid, so the bot starts in no_trade. A "*" symbol is a UNIVERSE config whose
// hard-limit validation is deferred to per-symbol expansion (configForSymbol),
// where the concrete whitelisted symbol is known.
func loadActiveConfigOrNil(path string, hl *config.HardLimits, logger *slog.Logger) *config.StrategyConfig {
	cfg, err := config.LoadStrategyConfig(path)
	if err != nil {
		logger.Info("no active strategy config; defaulting to no_trade", "path", path, "err", err)
		return nil
	}
	if cfg.Symbol != "*" {
		if err := cfg.ValidateAgainstHardLimits(hl); err != nil {
			logger.Warn("active strategy config rejected by hard limits; defaulting to no_trade", "err", err)
			return nil
		}
	}
	cfg.ActivatedAt = time.Now()
	return cfg
}

// isLoopbackAddr reports whether an HTTP listen address is loopback-only.
// On any parse ambiguity it returns false (treat as exposed → fail-close).
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // no port — treat the whole string as the host
	}
	if host == "" {
		return false // ":8090" = all interfaces
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// configForSymbol resolves a loaded config to a concrete symbol. A "*" universe
// config is cloned per symbol with a unique config_id (Postgres FK).
func configForSymbol(base *config.StrategyConfig, sym string) *config.StrategyConfig {
	return base.ForSymbol(sym)
}

// activeConfigRecord は arm 時に永続化する境界 record。同じ 6 フィールドを 3 箇所で
// 組み立てていたのを 1 か所に寄せた。
//
// 🛑 AdvisorRunID は**ここでは入れない**。advisor 由来の config だけが刻む値で
// (人手 / テスト / sentinel は空 = NULL 保存・config 凍結のため pg 側は COALESCE)、
// 一律に写すと由来の区別が消える。必要な呼び出し側だけが明示的に足す。
func activeConfigRecord(c *config.StrategyConfig) port.StrategyConfigRecord {
	return port.StrategyConfigRecord{
		ConfigID: c.ConfigID, Symbol: c.Symbol, Mode: string(c.Mode),
		StrategyName: string(c.StrategyName), Status: "active", RawYAML: c.YAMLText(),
	}
}

// warnDroppedUniverse は日次ユニバース(symbols_file)から allowed_symbols 外として外した銘柄を出す。
// 起動は止めない(減る方向 = fail-safe)。多くは「朝の選定の後に上場廃止で一覧から外した」銘柄。
func warnDroppedUniverse(logger *slog.Logger, track string, dropped []string) {
	if len(dropped) == 0 || logger == nil {
		return
	}
	logger.Warn("universe: 日次ユニバースに allowed_symbols 外の銘柄があったので外して起動する",
		"track", track, "dropped", dropped,
		"理由", "選定ファイル(07:00 の生成物)が一覧の変更より古い — 上場廃止などで一覧から外した銘柄")
}
