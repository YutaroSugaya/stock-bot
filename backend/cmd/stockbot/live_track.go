package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/symbolblock"
	"stockbot/backend/internal/app"
	"stockbot/backend/internal/app/handler"
	"stockbot/backend/internal/app/perfcycles"
	"stockbot/backend/internal/app/protectiveboard"
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

// liveTrack は hybrid の実弾トラック。research トラックとは **執行・台帳(DB)・
// リスク状態・emergency がすべて別**で、共有するのは立花のセッションと一括クォート
// フィードだけ。
//
// 🛑 research 側のコードは 1 行も通らない。「既定(env 未設定)は現行と完全に同一」
// という互換条件は、live track を**足すだけ**にすることで守っている。
type liveTrack struct {
	bundles  []*app.SymbolBundle
	loopCfg  loopConfig
	counters *app.Counters
	// emergency / store は shutdown と HTTP(Step 5)が読む。
	emergency *safety.EmergencyStop
	store     store
	botCfg    *config.BotConfig
	closeFn   func()

	// selector は決定論的な銘柄選定(nil = 静的 config)。rearmInterval ごとに
	// ランキングを引き直し、空き枠のぶんだけ arm し直す。
	selector      *app.Selector
	rearmInterval time.Duration
	// templateStrategies は live で回す戦略を **優先順位つき**で持つ(先頭が最優先)。
	// 単一戦略なら要素 1 つ = 従来と同じ。
	templateStrategies []config.StrategyName
	// broker は HTTP の個別決済が使う(bundles と同じインスタンス)。
	broker port.LiveBroker

	// replaceProtective は broker 側の守りの注文期日を切れる前に延ばす。
	// **live 専用** — paper には期日という概念が無い。
	//
	// 🚨 **訂正ではなく取消 → 再発注**。立花の「10営業日迄」は**発注日起点**なので、
	// 訂正では元の注文の天井を超えられない。訂正経路(CLMKabuCorrectOrder)は
	// そのため持たない。
	replaceProtective *command.ReplaceProtectiveOrder
	// 🚨 rearmProtective は **板から消えた守りを台帳の凍結値で置き直す**。
	// replaceProtective は板に注文が在ることが前提なので、失効して消えた守りは拾えない。
	rearmProtective *command.RearmUnguarded
	// raiseTrailStops は寄り前に、線が立っている trail 建玉の板の SL を利確の線まで引き上げる。
	// 期日の置き直しの後に同じ流れで回す(runReplaceProtectiveExpiry)。
	raiseTrailStops *command.RaiseTrailStops
	// protectiveMu は寄り前の手当て(置き直し → 引き上げ)と守りの自動復旧を直列にする鍵。
	// 取消と再発注の間に復旧が「守りが無い」と読んで置くと、再発注が拒否されて trip する。
	// 🛑 決済(closeOne)と人間の操作(HTTP)はこの鍵を取らない — 銘柄ごとの排他は持たない。
	protectiveMu sync.Mutex
	hours        session.TradingHours
	clock        clock.Clock
	// replaceState は守りの置き直しの観測。「受理されたか」を人間が確かめる手段が
	// 起動ログの目視だけだと足りないので、API に出して受入スクリプトから読めるようにする。
	replaceState protectiveReplaceState
	// boardState は **板にいま載っている守り**の写し。画面の TP/SL を台帳の凍結値
	// ではなく実体で出すために要る(人間がアプリで締めた値・守りが消えた状態)。
	boardState protectiveboard.State
	// symbolBlocks は人間の銘柄ごとの新規停止(ファイル)。**live だけ**が持つ。
	symbolBlocks port.SymbolBlockStore
}

// liveTrackDeps are the singletons the live track borrows from the process.
// **共有してよいのは「状態を持たないもの」と「立花のセッション」だけ** — counters /
// emergency / repo / pending を共有した瞬間、片側の事故が他側に伝播する。
type liveTrackDeps struct {
	hardLimits *config.HardLimits
	hours      session.TradingHours
	engine     *strategy.Engine
	clock      clock.Clock
	// candles は **市場の参照データ**なので両トラックで共有する。台帳(建玉・約定・
	// config)の物理分離とは別軸 — 日足を二重に持つと API/ディスクを浪費した上に、
	// 片方だけ古いという乖離を作る。🛑 これを live 自身の(空の)DB にすると、
	// selector はランキングできず **bot は健全に見えて永久に何も建てない**。
	candles  port.CandleRepository
	tb       port.LiveBroker // 素の立花(nil = 実ブローカーの無い構成)
	feed     port.MarketFeed // 共有一括フィード
	hotWatch *app.HotWatch
	logger   *slog.Logger
}

// accountCacheTTL は LiveQuoteShared の口座照会併合の窓。価格ループ 1 周期ぶん —
// 同じティックで複数銘柄が同じ口座照会を投げるのを畳むのが目的で、これより長くすると
// 「決済したのに建玉が残って見える」窓が広がる(状態変更時は即 invalidate されるが、
// broker 側で約定した決済はこちらから観測するしかない)。
const accountCacheTTL = 5 * time.Second

// buildLiveTrack assembles the live track, or returns nil when the opt-in env is
// absent. **nil, nil が既定** — hybrid のコードが入っていても何も起きない。

// newLiveEmergency は live の緊急停止を作る。
//
// 🚨 **onTrip を配線する。** 従来 live は nil を渡していたので、trip が**成功**した
// ときログ行もカウンタも出なかった(research は main.go でカウンタを増やしている)。
// 表面はダッシュボードのブール値と ~/.stockbot/state のフラグファイルだけ。
// 「人間が 1 時間以内に気付ける」ことが価値の全部である一連の修正にとって、
// ここが最弱点だった —— 「信号を誰も見ていなかった」形の事故になる。
//
// 🛑 カウンタは live 専用インスタンス(research と数字を混ぜない・CLAUDE.md)。
// 🛑 onTrip は EmergencyStop の mutex を握ったまま呼ばれる。logger は
// stdout + ファイルの MultiWriter なので同期 I/O が 1 行走る —— 年に数回・1 行なので
// 許容するが、ここに network I/O を足さないこと(全 Trip と Resume が止まる)。
//
// 発火条件(safety/emergency.go):
//
//	成功して**初めて**永続化できた回 … onTrip
//	永続化に失敗した回                … onTripErr
//	既に trip 済み                    … どちらも呼ばれない(先勝ちで理由を保つ)
func newLiveEmergency(flagPath string, counters *app.Counters, logger *slog.Logger) *safety.EmergencyStop {
	if counters == nil {
		counters = &app.Counters{} // nil のまま Add すると mutex を握ったまま panic する
	}
	return safety.NewEmergencyStop(flagPath, func(reason string) {
		counters.EmergencyTrips.Add(1)
		if logger != nil {
			logger.Error("🚨 LIVE EMERGENCY TRIP — 実弾の新規建てを停止した", "track", "live",
				"reason", reason, "復旧", "POST /api/live/emergency-resume(人間のみ)")
		}
	}).OnTripError(func(reason string, err error) {
		counters.EmergencyTrips.Add(1)
		if logger != nil {
			logger.Error("LIVE EMERGENCY TRIP COULD NOT BE PERSISTED — flag file unwritable",
				"track", "live", "reason", reason, "err", err)
		}
	})
}

func buildLiveTrack(ctx context.Context, e config.LiveTrackEnv, researchEnv config.Env, researchCfg *config.BotConfig, d liveTrackDeps) (*liveTrack, error) {
	// 🚨 **部分構成を落とす**。opt-in が bot_config だけなので、
	// 他の LIVE_* を書いてこれだけ忘れると live は警告も出さず無効化され、
	// 実弾建玉が誰にも監視されないまま残る。
	if err := e.Validate(); err != nil {
		return nil, err
	}
	if !e.Enabled() {
		// 🚨 明示の opt-out は**黙って**無効化しない。実弾建玉が OPEN のまま誰にも
		// 監視されない(守りの置き直しも max_hold も走らない)状態に入ることを叫ぶ。
		if e.Disabled && e.BotConfigPath != "" && d.logger != nil {
			d.logger.Warn("🛑 live track DISABLED by STOCKBOT_LIVE_DISABLED=1 — 実弾建玉が OPEN なら誰にも監視されない(守りの置き直し・max_hold も走らない)",
				"track", "live", "bot_config", e.BotConfigPath)
		}
		return nil, nil
	}
	botCfg, err := loadLiveBotConfig(e, researchEnv, researchCfg, d)
	if err != nil {
		return nil, err
	}
	brk, err := liveTrackBroker(botCfg, d, e)
	if err != nil {
		return nil, err
	}

	st, err := buildLiveStore(ctx, e.DatabaseURL, d.logger)
	if err != nil {
		return nil, err
	}
	lt, err := assembleLiveTrack(ctx, e, botCfg, brk, st, d)
	if err != nil {
		st.closeFn()
		return nil, err
	}
	return lt, nil
}

// loadLiveBotConfig loads the live yaml and runs every start-up validation the
// research track runs, plus the hybrid-specific ones (mode / broker / DSN / flag
// separation, no advisor on the order path).
func loadLiveBotConfig(e config.LiveTrackEnv, researchEnv config.Env, researchCfg *config.BotConfig, d liveTrackDeps) (*config.BotConfig, error) {
	botCfg, err := config.LoadBotConfig(e.BotConfigPath)
	if err != nil {
		return nil, fmt.Errorf("live track: %w", err)
	}
	if err := botCfg.ResolveSymbols(); err != nil {
		return nil, fmt.Errorf("live track: %w", err)
	}
	// 2 トラック構成そのものの検証(mode / broker / DSN / パスの分離)。
	if err := config.ValidateHybrid(researchCfg, botCfg, researchEnv, e); err != nil {
		return nil, err
	}
	// 🛑 research 側と**同じ**起動時検証を live 側 yaml にも通す。現行はこれらを
	// 単一 config 前提で 1 回だけ呼んでいるので、素直に 2 トラックにすると live 側が
	// 素通りする(HYBRID Step 1)。
	warnDroppedUniverse(d.logger, "live", botCfg.DropSymbolsOutsideWhitelist(d.hardLimits))
	if err := botCfg.ValidateAgainstHardLimits(d.hardLimits); err != nil {
		return nil, fmt.Errorf("live track: %w", err)
	}
	if err := botCfg.ValidateBrokerCapabilities(); err != nil {
		return nil, fmt.Errorf("live track: %w", err)
	}
	if err := config.RequireLiveGuards(botCfg.Mode); err != nil {
		return nil, fmt.Errorf("live track: %w", err)
	}
	if err := config.RequireDurableBackend(botCfg.Mode, botCfg.Broker.Kind, e.DatabaseURL); err != nil {
		return nil, fmt.Errorf("live track: %w", err)
	}
	// 🛑 **LLM をリアルタイム発注経路に入れない**(CLAUDE.md)。禁じるのは advisor だけ。
	//
	// Selector(決定論的なスキャン + arm)は許す。CLAUDE.md が縛るのは LLM であって、
	// 「どの銘柄に当てるか」を日足から機械的に決めることではない。「人間 commit 固定」は
	// **どの戦略を回すか**の話で、それはテンプレート戦略が live_allowed_strategies に
	// 入っていること(下の runtime 強制)で担保される。
	if botCfg.Advisor.Enabled {
		return nil, fmt.Errorf("live track で advisor.enabled=true は不可(LLM をリアルタイム発注経路に入れない — CLAUDE.md)")
	}
	return botCfg, nil
}

// assembleLiveTrack wires the ledger-backed pieces: paper-book restore, the live
// emergency stop, per-symbol bundles and the tiered selector. The caller closes
// the store on error.
func assembleLiveTrack(ctx context.Context, e config.LiveTrackEnv, botCfg *config.BotConfig, brk port.LiveBroker, st store, d liveTrackDeps) (*liveTrack, error) {
	// 🛑 dry-run(紙 broker)のときだけ紙帳簿を復元する。実ブローカーでは建玉の正本は
	// broker 側なので adoptPaperBook は no-op に落ちる(型で判定している)。
	if err := adoptPaperBook(ctx, brk, st.positions, d.logger); err != nil {
		return nil, fmt.Errorf("live track: %w", err)
	}

	counters := &app.Counters{}
	emergency := newLiveEmergency(e.EmergencyFlagPath, counters, d.logger)
	if err := emergency.Preflight(); err != nil {
		return nil, fmt.Errorf("live track: %w", err)
	}

	blocks := newLiveSymbolBlocks(ctx, e, d)
	deps := &wiringDeps{
		broker: brk, posRepo: st.positions, tradeRepo: st.trades, closer: st.closer, candles: d.candles,
		rejections: st.rejections, pending: safety.NewPendingPositions(), emergency: emergency,
		engine: d.engine, hours: d.hours, clock: d.clock, botCfg: botCfg, hardLimits: d.hardLimits,
		counters: counters, logger: d.logger, symbolBlocks: blocks,
	}

	loopCfg := defaultLoopConfig(botCfg.Mode, botCfg.Broker.Kind, broker.QuotesBatched(brk))
	selectorOn := botCfg.Selector.Enabled
	actives, err := liveTemplates(e, d, selectorOn)
	if err != nil {
		return nil, err
	}
	bundles, holders, err := liveBundles(ctx, botCfg, st, deps, actives, selectorOn, loopCfg, d)
	if err != nil {
		return nil, err
	}

	lt := &liveTrack{
		bundles: bundles, loopCfg: loopCfg, counters: counters, broker: brk,
		emergency: emergency, store: st, botCfg: botCfg, closeFn: st.closeFn,
		hours: d.hours, clock: d.clock, symbolBlocks: blocks,
		// 🛑 守りの注文期日を切れる前に延ばす。**live だけ**に付ける —
		// paper broker には期日が無く、no-op を回しても意味が無い。
		// 場外ガードは usecase 側にもある(二重に持つ・loops.go の窓と対)。
		replaceProtective: command.NewReplaceProtectiveOrder(st.positions, brk, d.hours, d.clock, emergency).
			// 🚨 前日の板の TP/SL を**今日の帯**で検問してから取り消す。
			WithPriceLimitRef(lastDailyClose(d.candles)).
			WithLogger(func(msg string, kv ...any) { d.logger.Info(msg, append([]any{"track", "live"}, kv...)...) }),
		// 🚨 板から消えた守りの自動復旧。**live だけ** — paper の板に失効は無い。
		// 🛑 emergency を渡さない: 守りを置くのは新規建てではないので止めない
		// (裸の建玉を放置するほうが危険。arm と同じ規律)。
		rearmProtective: command.NewRearmUnguarded(st.positions, brk,
			command.NewArmProtectiveOrder(st.positions, brk, d.hours, d.clock).
				WithPriceLimitRef(lastDailyClose(d.candles))).
			WithLogger(func(msg string, kv ...any) { d.logger.Info(msg, append([]any{"track", "live"}, kv...)...) }),
		// 🚨 trail の利確の線を板の SL にも置く。取消 → 再発注・帯の検問・台帳の更新は
		// 人間の値段変更と同じ RepriceProtectiveOrder。emergency を渡す(再発注の失敗 = 裸 → trip)。
		raiseTrailStops: command.NewRaiseTrailStops(st.positions, brk, d.hours, d.clock,
			command.NewRepriceProtectiveOrder(st.positions, brk, d.hours, d.clock, emergency).
				WithPriceLimitRef(lastDailyClose(d.candles)).
				WithLogger(func(msg string, kv ...any) { d.logger.Info(msg, append([]any{"track", "live"}, kv...)...) })).
			WithPriceLimitRef(lastDailyClose(d.candles)),
	}
	if selectorOn {
		// 🛑 live は **1銘柄1 config のまま**。ConfigSet の base holder を渡す。
		sel, err := buildLiveSelector(ctx, botCfg, actives, baseHolders(holders), st, d, deps.accountMargin(), blocks)
		if err != nil {
			return nil, err
		}
		lt.selector = sel
		for _, a := range actives {
			lt.templateStrategies = append(lt.templateStrategies, a.StrategyName)
		}
		lt.rearmInterval = time.Duration(botCfg.Selector.RearmIntervalMin) * time.Minute
		if lt.rearmInterval <= 0 {
			lt.rearmInterval = time.Hour
		}
		// 優先順位と**ティアごとの**建玉金額上限をそのままログに出す。1 つの数字で
		// 書けない構成なので、起動ログが唯一「どの順で何円まで拾うか」を示す場所になる。
		caps := make([]any, 0, len(actives)*2)
		for i, a := range actives {
			caps = append(caps, fmt.Sprintf("tier%d_%s_max_notional_jpy", i+1, a.StrategyName),
				botCfg.Selector.NotionalCapFor(a.StrategyName))
		}
		d.logger.Info("live selector enabled",
			append([]any{
				"templates_by_priority", lt.templateStrategies,
				"universe", len(bundles), "account_max", botCfg.Risk.AccountMaxOpenPositions,
				"max_risk_per_trade_jpy", botCfg.Risk.MaxRiskPerTradeJPY,
				"rearm_interval", lt.rearmInterval,
			}, caps...)...)
	}

	d.logger.Info("live track enabled", "mode", botCfg.Mode, "broker", botCfg.Broker.Kind,
		"symbols", len(bundles), "dry_run", e.DryRun,
		"max_gross_notional_ratio", botCfg.Risk.MaxGrossNotionalRatio,
		"db", "separate", "emergency_flag", e.EmergencyFlagPath)

	return lt, nil
}

// liveTemplates loads the strategy templates in priority order and, when the
// selector arms them, fails closed on any template that may not run live.
func liveTemplates(e config.LiveTrackEnv, d liveTrackDeps, selectorOn bool) ([]*config.StrategyConfig, error) {
	// 🛑 **並び順がそのまま優先順位**(先頭が最優先)。`STOCKBOT_LIVE_STRATEGY_CONFIG` を
	// カンマ区切りにする。1 本なら従来と完全に同じ。
	var actives []*config.StrategyConfig
	for _, p := range e.StrategyConfigPaths() {
		if c := loadActiveConfigOrNil(p, d.hardLimits, d.logger); c != nil {
			actives = append(actives, c)
		}
	}
	if selectorOn {
		if len(actives) == 0 {
			return nil, fmt.Errorf("live track: selector.enabled には出口を自前計算する戦略のテンプレートが要る(STOCKBOT_LIVE_STRATEGY_CONFIG)")
		}
		// 🛑 **1 本でも欠けたら起動しない**(fail-close)。「2 本指定したのに 1 本しか
		// 効いていない」を黙って許すと、下位ティアが永久に arm されないまま緑に見える。
		seen := map[config.StrategyName]bool{}
		for _, active := range actives {
			if !strategy.LiveArmable(active.StrategyName) {
				return nil, fmt.Errorf("live track: selector.enabled には出口を自前計算する戦略のテンプレートが要る(STOCKBOT_LIVE_STRATEGY_CONFIG); got %q", strategyNameOrNone(active))
			}
			if !d.engine.Has(active.StrategyName) {
				return nil, fmt.Errorf("live track: selector template strategy %q is not registered", active.StrategyName)
			}
			// 🛑 ここが「どの戦略を実弾で回すか」の人間ゲート。selector 経由でも必ず通す。
			if !d.hardLimits.AllowsLiveStrategy(active.StrategyName) {
				return nil, fmt.Errorf("live track refuses selector template strategy %q: not in hard_limits live_allowed_strategies (edge-proof promotion is a human commit)", active.StrategyName)
			}
			// 🛑 同じ戦略を 2 つのティアに置かない。優先順位が自分自身と競合して、
			// どちらの上限が効くのかが読めなくなる。
			if seen[active.StrategyName] {
				return nil, fmt.Errorf("live track: selector template strategy %q が重複している(優先ティアは戦略ごとに 1 つ)", active.StrategyName)
			}
			seen[active.StrategyName] = true
		}
	}

	return actives, nil
}

// liveBundles builds one SymbolBundle (+ config holder) per watched symbol.
// Positions outside today's universe are watched for exits only (no holder).
func liveBundles(ctx context.Context, botCfg *config.BotConfig, st store, deps *wiringDeps,
	actives []*config.StrategyConfig, selectorOn bool, loopCfg loopConfig, d liveTrackDeps) ([]*app.SymbolBundle, map[string]*app.ConfigSet, error) {
	// 🚨 **建玉のある銘柄を必ず監視対象へ合流する**(research の main.go と同じ作法)。live だけ `botCfg.Symbols` を直接回していたので、
	// **日次ユニバースから外れた銘柄の実弾建玉が丸ごと管理外**になっていた。
	// symbols_file は毎朝 07:00 に書き換わるので、
	// 建玉中の銘柄が翌日 200 位から落ちるのは普通に起きる。管理外になると
	// ①OnTick が走らない = 台帳の TP / ratchet / max_hold が誰にも評価されない
	// ②ReconcileTick も走らない = broker 側の逆指値が約定しても台帳に決済が書かれず、
	// 建玉が永久に OPEN のまま枠と日次損失の計算を狂わせる。
	watched, adoptedSyms, err := watchedSymbols(ctx, st.positions, botCfg.Symbols)
	if err != nil {
		return nil, nil, fmt.Errorf("live track: %w", err)
	}
	if len(adoptedSyms) > 0 {
		d.logger.Warn("live: ユニバース外の建玉を監視対象へ合流した(決済のためだけ・arm はしない)",
			"symbols", adoptedSyms)
	}
	adopted := make(map[string]bool, len(adoptedSyms))
	for _, s := range adoptedSyms {
		adopted[s] = true
	}
	bundles := make([]*app.SymbolBundle, 0, len(watched))
	holders := make(map[string]*app.ConfigSet, len(watched))
	for _, sym := range watched {
		// selector が回すときは **全銘柄 no_trade から始める**。arm するのは
		// ランキング上位かつ資金条件を満たしたものだけ(Selector.Tick)。
		cfg := defaultNoTradeConfig(sym, botCfg.Mode)
		// 🛑 合流ぶんは **no_trade 固定**。ユニバース外の銘柄で新規に建てないため
		// (決済経路だけを開ける)。active config の適用も allowlist 検査も通さない。
		// 🛑 selector 無し(静的 config)の経路は **先頭のテンプレートだけ**を使う。
		// 優先ティアは selector の仕組みなので、selector を切ったまま複数テンプレートを
		// 並べても意味を持たない(2 本目以降は無視される)。
		if cand := configForSymbol(firstTemplate(actives), sym); !selectorOn && cand != nil && !adopted[sym] {
			if err := cand.ValidateAgainstHardLimits(d.hardLimits); err != nil {
				// live は黙って no_trade に落とさない: 実弾を出すつもりの config が
				// 通らないなら、それは構成ミスであって「今日は様子見」ではない。
				return nil, nil, fmt.Errorf("live track: active config for %q rejected: %w", sym, err)
			}
			cfg = cand
		}
		if !d.engine.Has(cfg.StrategyName) {
			return nil, nil, fmt.Errorf("live track: active config for %q names unregistered strategy %q", sym, cfg.StrategyName)
		}
		// live 戦略 allowlist の runtime 強制(昇格は人間 commit のみ)。
		if !d.hardLimits.AllowsLiveStrategy(cfg.StrategyName) {
			return nil, nil, fmt.Errorf("live track refuses strategy %q for %q: not in hard_limits live_allowed_strategies (edge-proof promotion is a human commit)", cfg.StrategyName, sym)
		}
		if st.activateCfg != nil {
			if err := st.activateCfg(activeConfigRecord(cfg)); err != nil {
				return nil, nil, fmt.Errorf("live track: persist active config %q: %w", cfg.ConfigID, err)
			}
		}
		b := buildSymbolBundle(deps, sym, cfg)
		b.HotWatch = d.hotWatch
		if botCfg.Broker.Kind == config.BrokerTachibana {
			b.MarginPollInterval = loopCfg.marginHeldInterval
		}
		if selectorOn {
			// armed / 建玉中の銘柄だけ broker を poll する(200銘柄でも時価以外は増えない)。
			b.MonitorNarrow = true
		}
		bundles = append(bundles, b)
		// 🛑 **合流ぶんは holders に載せない** = selector が arm できない。載せると
		// ユニバース外の銘柄に**新規の実弾建玉**を出せてしまう(合流の目的は決済だけ)。
		if !adopted[sym] {
			holders[sym] = b.Configs
		}
	}

	return bundles, holders, nil
}

// httpViews は live トラックの HTTP 面(Step 5)。**research の束と別インスタンス**を
// 返すのが唯一の目的で、混ぜた瞬間に損益が混読される。
//
// 🛑 flatten-all は含めない。live の全清算を人間がボタン 1 つでやる操作にしない。
func (lt *liveTrack) httpViews(hl *config.HardLimits, hours session.TradingHours, clk clock.Clock,
	candles port.CandleRepository, brk port.LiveBroker, logger *slog.Logger) *handler.LiveViews {
	statusQ := query.NewGetBotStatus(lt.store.positions, lt.emergency,
		string(lt.botCfg.Mode), string(lt.botCfg.Broker.Kind))
	if lt.store.strategies != nil {
		statusQ = statusQ.WithStrategyResolver(lt.store.strategies)
	}
	bySym := make(map[string]*app.SymbolBundle, len(lt.bundles))
	for _, b := range lt.bundles {
		bySym[b.Symbol] = b
	}
	// 🛑 research 側の「警報なし」規約は流用しない。あれは「paper は資本リスク
	// ゼロ」という理由に依存していて live に移植できない。
	//
	// 🚨 **裸の建玉だけを拾う警報を渡す**。画面の成行決済は
	// 守りの脚を cancel してから決済を出すので、決済が受理されたのに板に載らないと
	// 建玉は逆指値も決済注文も持たない。live の reconcile は保有中でも 1 時間おきで、
	// bot 側にこの窓を縮める手段が無い。
	// 🛑 渡すのは最後の引数(unprotected)であって emergency ではない。close **拒否**で
	// live を止めない事前コミットは維持する(拒否は CLOSING のまま reconcile が回収)。
	closeCmd := command.NewCloseAllOpen(brk, lt.store.positions, lt.store.closer,
		func(symbol string) (*market.MarketSummary, *market.MarketSummary) {
			b, ok := bySym[symbol]
			if !ok {
				return nil, nil
			}
			return b.LastSummary(), b.LastIndicative()
		}, clk, lt.emergency).
		WithCarry(hl.CarryCalc(hours)).
		WithLogger(logger.Info)

	// UI 集計は live 専用に組む。research の scanProvider / counters を使い回すと
	// 画面上でどちらの数字か区別できなくなる。
	scan := app.NewScanProvider(candles, lt.botCfg.Symbols, screenersForTrack(lt), 30*time.Second, clk)
	extra := lt.dashboardExtra(scan, logger)

	perfTable := loadPerfCycles(logger)
	return &handler.LiveViews{
		Extra: extra,
		// 🛑 銘柄ごとの新規停止(人間のボタン)。銘柄は allowed_symbols に載っているものだけ受ける。
		BlockSymbol:   command.NewBlockLiveSymbol(lt.symbolBlocks, hl.AllowsSymbol),
		ReleaseSymbol: command.NewReleaseLiveSymbol(lt.symbolBlocks),
		SymbolBlocks:  query.NewListSymbolBlocks(lt.symbolBlocks),
		Status:        statusQ,
		ListOpen:      query.NewListOpenPositions(lt.store.positions),
		Performance:   query.NewBuildForwardReport(lt.store.trades, perfReportOptions(lt.store.strategies)...),
		// 戦績のサイクル切り替え。live DB は 1 本なので期間で切るだけ(app/perfcycles)。
		Cycles:       perfcycles.BuildLive(perfTable.Cycles),
		CycleDefault: perfTable.Current,
		Emergency:    lt.emergency,
		Close:        closeCmd,
		// 🛑 live 専用インスタンス。research の extendCmd を使い回すと research の
		// id 空間で実弾建玉を伸ばせてしまう(台帳が別 DB なので id は衝突する)。
		Extend: command.NewExtendMaxHold(lt.store.positions, lt.emergency),
		// 延長先の候補(各営業日の引け前)。休場日を知っているのは hours だけなので
		// 画面に日付を計算させない。
		ExtendOptions: query.NewListExtendOptions(lt.store.positions, hours),
		// 🛑 守りの**取消 → 再発注**(期日の延長)。訂正では発注日+10営業日の天井を
		// 超えられないので、これが唯一の延長手段。
		// **叩いたときだけ走る** — 自動化の前に人間が見ている前で 1 回試すため。
		// 場中は Execute 自身が no-op に倒す(取消と再発注の間に守りが消える窓)。
		ReplaceProtective: func(ctx context.Context) (int, int, []string) {
			res, errs := lt.replaceProtective.Execute(ctx)
			lt.replaceState.set(clk(), res, errs)
			msgs := make([]string, 0, len(errs))
			for _, e := range errs {
				logger.Error("守りの置き直しに失敗", "track", "live", "err", e)
				msgs = append(msgs, e.Error())
			}
			return res.Replaced, res.Skipped, msgs
		},
		// 🚨 守りの**新規設置**。板に守りが無い建玉を救う唯一の経路。
		// 置き直しは板に注文が在ることが前提なので、取消が通った後で再発注が失敗した
		// 状態からは復旧できない —— live の建玉が寄り前に裸で残ったとき
		// コードにできることが何も無かった。
		// 🛑 emergency 中でも撃てる(守りを置くのは新規建てではない)。
		ArmProtective: func(ctx context.Context, symbol string, tp, sl float64) (string, error) {
			id, err := command.NewArmProtectiveOrder(lt.store.positions, brk, hours, clk).
				WithLogger(func(msg string, kv ...any) { logger.Info(msg, append([]any{"track", "live"}, kv...)...) }).
				// 🚨 値幅制限の基準値段(前日終値)を挿す。挿さないと帯の判定をしないので、
				// 帯の外の脚をそのまま送って**注文ごと拒否 = 裸のまま**になる。
				WithPriceLimitRef(lastDailyClose(candles)).
				Execute(ctx, command.ArmProtectiveInput{Symbol: symbol, TakeProfit: tp, StopLoss: sl})
			if err != nil {
				logger.Error("守りの設置に失敗", "track", "live", "symbol", symbol, "err", err)
			}
			return id, err
		},
		// 🚨 守りの**値段**を変える(取消 → 再発注)。場中でも走る —— 取消と再発注の
		// 間に守りが消える窓が開くが、人間の操作なので承知の上で開ける。
		RepriceProtective: func(ctx context.Context, symbol string, tp, sl float64) (string, error) {
			id, err := command.NewRepriceProtectiveOrder(lt.store.positions, brk, hours, clk, lt.emergency).
				// 🚨 呼値と今日の帯を**取消の前に**検問する。
				WithPriceLimitRef(lastDailyClose(candles)).
				WithLogger(func(msg string, kv ...any) { logger.Info(msg, append([]any{"track", "live"}, kv...)...) }).
				Execute(ctx, command.RepriceProtectiveInput{Symbol: symbol, TakeProfit: tp, StopLoss: sl})
			if err != nil {
				logger.Error("守りの値段変更に失敗", "track", "live", "symbol", symbol, "err", err)
			}
			return id, err
		},
		ActiveBySym: func() map[string]string {
			m := make(map[string]string, len(lt.bundles))
			for _, b := range lt.bundles {
				m[b.Symbol] = strings.Join(b.Configs.ConfigIDs(), ",")
			}
			return m
		},
	}
}

// dashboardExtra is the live dashboard aggregate: counters, quotes, selector
// state and the protective-order board, all from live-only instances.
// ledgerTotalView は累計(決済の net + 建玉の含み)。ダッシュボードの累計欄に出す。
// 含みは直近の約定値で評価し、時価の無い銘柄は名指しで返す。
func (lt *liveTrack) ledgerTotalView() any {
	bySym := make(map[string]*app.SymbolBundle, len(lt.bundles))
	for _, b := range lt.bundles {
		bySym[b.Symbol] = b
	}
	v, err := query.NewLedgerTotal(lt.store.trades, lt.store.positions).Execute(context.Background(),
		func(sym string) (float64, bool) {
			b := bySym[sym]
			if b == nil {
				return 0, false
			}
			s := b.LastSummary()
			if s == nil || s.CurrentRate.Last <= 0 {
				return 0, false
			}
			return s.CurrentRate.Last, true
		})
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return v
}

func (lt *liveTrack) dashboardExtra(scan *app.ScanProvider, logger *slog.Logger) func() map[string]any {
	gonogoPanel := newGoNoGoPanel("live", nil, lt.clock, logger)
	return func() map[string]any {
		armed := ""
		var armedSyms []string
		// -1 = 未計測(selector 無し / 初回 Tick 前)。0(= 全銘柄が上限超)と区別する。
		affordable, maxNotional := -1, lt.botCfg.Selector.MaxPositionNotionalJPY
		levState, levHeadroom := "", 0
		if lt.selector != nil {
			armedSyms = lt.selector.ArmedSymbols()
			armed = lt.selector.ArmedSymbol()
			affordable = lt.selector.AffordableCount()
			maxNotional = lt.selector.MaxNotionalJPY()
			levState = string(lt.selector.LeverageState())
			levHeadroom = lt.selector.LeverageHeadroomJPY()
		}
		openN, _ := lt.store.positions.CountOpenAllSymbols(context.Background())
		ranking := app.RankingView(context.Background(), scan, lt.bundles, lt.store.positions, lt.selector)
		return map[string]any{
			// 寄り前の go/no-go(表示だけ・止めるのは人間の「停止」ボタン)。
			"gonogo":    gonogoPanel(ranking),
			"counters":  lt.counters.Snapshot(),
			"summaries": quoteViews(lt.bundles),
			// 累計(決済の net + 建玉の含み)。
			"ledger_total": lt.ledgerTotalView(),
			"selector": map[string]any{
				"enabled":       lt.selector != nil,
				"armed_symbol":  armed,
				"armed_symbols": armedSyms,
				"account_open":  openN,
				"account_max":   lt.botCfg.Risk.AccountMaxOpenPositions,
				"universe":      len(lt.botCfg.Symbols),
				// universe のうち **1単元が建玉金額上限に収まる**銘柄数。上限超の銘柄は
				// 発火しても arm されない = 買われないので、内数を出さないと画面の
				// 「スキャン対象 N」が実際の対象より大きく読める。
				"affordable":       affordable,
				"max_notional_jpy": maxNotional,
				// 🚨 **なぜ 1 本も arm されていないのか**を画面が名指しできるようにする。
				// "fundless" = 資金枠なし(枠待ち)/ "unknown" = 口座照会
				// が読めない(fail-close で arm しない)/ "ok" = 余力あり。
				// headroom は 上限 − 建玉合計 で、**負の値もそのまま出す**(枠待ちの深さ)。
				"leverage_state":        levState,
				"leverage_headroom_jpy": levHeadroom,
				"ranking":               ranking,
			},
			// 🛑 live に advisor は存在しない(LLM を発注経路に入れない)。UI が
			// 空パネルを出して「今日は判断が無かった」と読まれないよう、理由を返す。
			// 🚨 守りの置き直し(期日が近い守りの取消 → 再発注)の結果。受理されたかを人間が確かめる面。
			// キー名は PROTECTIVE_EXPIRY_RUNBOOK の手順が読むので変えない。
			"protective_renew": lt.replaceState.view(),
			// 🚨 **板の実体**。画面の TP/SL はこちらを主に出す — 台帳の凍結値は
			// 建てたときの値で、人間がアプリで締めた後は嘘になる。
			"protective_board":        lt.boardState.View(),
			"advisor_disabled_reason": "live track では advisor(LLM)は構成として無効 — LLM をリアルタイム発注経路に入れない(CLAUDE.md)",
		}

	}
}

// firstTemplate は先頭(最優先)のテンプレート。空なら nil。
func firstTemplate(tmpls []*config.StrategyConfig) *config.StrategyConfig {
	if len(tmpls) == 0 {
		return nil
	}
	return tmpls[0]
}

// screenersForTrack は live のスキャン表示に使う screener。selector が回っていれば
// **全テンプレート戦略ぶん**(= 実際に arm され得る戦略だけを画面に出す)。
//
// 🛑 ここは `ValidateSelectorScreeners` の制約(1 ティア 1 戦略)の対象外。あちらは
// **score を跨いで降順に並べるな**という話で、こちらは表示用に戦略ごとへ
// グルーピングする(`groupRanking`)だけなので複数渡してよい。
func screenersForTrack(lt *liveTrack) []strategy.Screener {
	if lt.selector == nil {
		return nil
	}
	var out []strategy.Screener
	for _, name := range lt.templateStrategies {
		out = append(out, strategy.LiveArmScreeners(name)...)
	}
	return out
}

// newLiveSymbolBlocks は live の銘柄ごとの新規停止の store(ファイル)。
// 読めない・壊れていても起動は止めない — 決済と守りは動かし続け、新規だけを全部止める
// (ゲートと selector が fail-close)。起動時に WARN を出し、画面にも出る。
func newLiveSymbolBlocks(ctx context.Context, e config.LiveTrackEnv, d liveTrackDeps) *symbolblock.FileStore {
	store := symbolblock.NewFileStore(e.SymbolBlocksFile(), d.clock)
	blocks, err := store.List(ctx)
	if d.logger == nil {
		return store
	}
	if err != nil {
		d.logger.Warn("🛑 live の銘柄の停止を読めない — 直すまで live の新規を全部止める(決済と守りは動く)",
			"track", "live", "path", store.Path(), "err", err)
		return store
	}
	syms := make([]string, 0, len(blocks))
	for _, b := range blocks {
		syms = append(syms, b.Symbol)
	}
	d.logger.Info("live symbol blocks", "track", "live", "path", store.Path(), "blocked", syms)
	return store
}

// buildLiveSelector wires the DETERMINISTIC universe selector for the live track:
// 日足で全銘柄をランキングし、資金条件(1単元の金額上限)を満たす上位から、
// 空いている枠のぶんだけ arm する。**LLM は一切関与しない。**
//
// arm する config はテンプレート(人間 commit)を銘柄ごとに複製したもので、戦略も
// 出口の geometry も人間が決めた 1 通りしかない。selector が決めるのは
// **どの銘柄に当てるか**だけ。
func buildLiveSelector(ctx context.Context, botCfg *config.BotConfig, tmpls []*config.StrategyConfig,
	holders map[string]*app.ActiveConfigHolder, st store, d liveTrackDeps,
	margin *command.AccountMarginCache, blocks port.SymbolBlockReader) (*app.Selector, error) {
	noTradeCfg := func(sym string) *config.StrategyConfig { return defaultNoTradeConfig(sym, botCfg.Mode) }
	persist := func(c *config.StrategyConfig) error {
		if st.activateCfg == nil {
			return nil
		}
		return st.activateCfg(activeConfigRecord(c))
	}
	// 🛑 **並び順 = 優先順位**。前のティアが空き枠を取り切ったら後ろは 1 本も arm されない。
	tiers := make([]app.SelectorTier, 0, len(tmpls))
	for _, raw := range tmpls {
		tmpl := liveArmTemplate(raw)
		screeners := strategy.LiveArmScreeners(tmpl.StrategyName)
		// 1 ティアには 1 戦略しか入れない(score は戦略間で比較不能)。
		if err := app.ValidateSelectorScreeners(screeners); err != nil {
			return nil, fmt.Errorf("live track: %w", err)
		}
		tiers = append(tiers, app.SelectorTier{
			Screeners: screeners,
			ArmCfg: func(sym string) *config.StrategyConfig {
				c := configForSymbol(tmpl, sym)
				if c == nil {
					return nil
				}
				if err := c.ValidateAgainstHardLimits(d.hardLimits); err != nil {
					d.logger.Warn("live selector: armed config rejected by hard limits",
						"symbol", sym, "strategy", string(tmpl.StrategyName), "err", err)
					return nil
				}
				return c
			},
			MaxNotionalJPY: botCfg.Selector.NotionalCapFor(tmpl.StrategyName),
		})
	}
	sel := app.NewSelectorTiered(d.candles, st.positions, holders, tiers,
		botCfg.Risk.AccountMaxOpenPositions, noTradeCfg, persist, d.logger)
	// 🚨 **1本あたりの計画損失の上限を arm の側にも挿す。** 発注前ゲート
	// (risk の `risk_per_trade`)だけだと、上限超の候補が毎 Tick 同じ首位として
	// arm され続けて口座の建玉枠を**永久に**占有する(分割未調整の銘柄が起こす
	// 沈黙のデッドロックと同じ形)。挿し忘れても実弾は出ない(ゲートが落とす)が、枠が空かない。
	sel.WithMaxRiskPerTradeJPY(botCfg.Risk.MaxRiskPerTradeJPY)
	// 🚨 **口座の資金枠を arm の前提条件にする**。余力が候補 1 本の
	// 金額を下回る口座でその候補を arm し、場中ずっと gross_notional_cap で落ち続けて
	// wire を数千回焼いた。読む値は **entry 経路と同じキャッシュ**なので、
	// selector が回るたびに立花を叩くことにはならない。
	sel.WithLeverageHeadroom(margin, botCfg.Risk.MaxGrossNotionalRatio)
	// 人間が止めた銘柄を arm しない(発注前ゲート `manual_symbol_block` の先出し)。
	// 最初の Tick より前に挿す — 後から挿すと起動直後の 1 周だけ止めた銘柄が枠を取る。
	sel.WithSymbolBlocks(blocks)
	// 発火した候補を arm しなかった理由を signal_rejections に残す(`selector_*`)。
	// 計画損失の上限で落ちた候補は、どこにも記録が無いと特定に日足の手計算が要る。
	sel.WithRejectionSink(st.rejections, time.Now)
	sel.Tick(ctx) // 起動直後に 1 周(寄りを待たない)
	return sel, nil
}

// liveTrackBroker picks the live track's execution path. **ここが実弾の入口**なので、
// 分岐は 2 本だけにして読める形に保つ。
func liveTrackBroker(botCfg *config.BotConfig, d liveTrackDeps, e config.LiveTrackEnv) (port.LiveBroker, error) {
	if botCfg.Broker.Kind != config.BrokerTachibana {
		// dry-run(Stage 1): 配線だけ本番形にして紙で回す。ValidateHybrid が
		// production との併用を既に拒否している。
		return newPaperBroker(d.hardLimits, d.clock, true /*seedPrices*/), nil
	}
	if d.tb == nil || d.feed == nil {
		// research 側が paper 単体だと共有する立花が存在しない。**2 本目の login を
		// ここで張らない** — 立花のセッションは口座に 1 本で、後から張った方が
		// 前のを破棄して互いに蹴り合う(却下案そのもの)。
		return nil, fmt.Errorf("live track に tachibana を指定したが、research 側が実ブローカーを持っていない(broker.kind=paper_live_feed か tachibana にして立花セッションを共有する)")
	}
	return broker.NewLiveQuoteShared(d.tb, d.feed, accountCacheTTL, d.clock), nil
}

// liveArmTemplate は arm 用テンプレの config_id を**中身に縛る**。
//
// strategy_configs.raw_yaml は凍結(既存行を上書きしない)なので、「同じ config_id は
// 永久に同じ内容」が崩れると台帳が嘘になる。テンプレの config_id は人間が yaml に
// 書く固定文字列で、中身を直しても変わらない — そこに指紋を足して、内容が変われば
// 必ず別 id になるようにする。人間が読める接頭辞は残す
// (`live_bnf_probe_v1_3f9c2a10_4751` のように、最後は従来どおり銘柄)。
//
// テンプレ自身は書き換えず複製を返す(呼び出し側が持つ値の同一性を壊さない)。
func liveArmTemplate(tmpl *config.StrategyConfig) *config.StrategyConfig {
	if tmpl == nil {
		return nil
	}
	c := *tmpl
	c.ConfigID = tmpl.ConfigID + "_" + tmpl.Fingerprint()
	return &c
}

// protectiveReplaceState は守りの置き直し(command.ReplaceProtectiveOrder)の観測(read-only)。
// JSON のキー(`renewed` / `renewed_total` 等)は PROTECTIVE_EXPIRY_RUNBOOK が読むので変えない。
//
// 🛑 **最後の周回だけを持つと、成功の証拠がその日のうちに消える。**
// 周期で回るが、置き直しが要る建玉は 1 度置き直せば次の周回では対象外になる。
// つまり **成功した直後の周回で renewed が 0 に戻る**。人間が引け後に見るのは
// その 0 のほうなので、「受理された」と「一度も呼ばれていない」が区別できなくなる
// (受入スクリプトが errors==0 だけで緑を出す形になる)。
// 失敗側も同じで、対象が消えた周回が来るとエラーが 0 に戻って合格に化ける。
// だから **通算と最後に起きた時刻は消さずに持つ**。
type protectiveReplaceState struct {
	mu    sync.Mutex
	ranAt time.Time

	// 最後の周回ぶん(いま何が起きているか)
	renewed  int
	skipped  int
	errCount int

	// 通算(消えない証拠)
	totalRenewed int
	lastRenewAt  time.Time
	totalErrors  int
	lastErr      string
	lastErrAt    time.Time
}

func (s *protectiveReplaceState) set(now time.Time, res command.ReplaceProtectiveResult, errs []error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ranAt, s.renewed, s.skipped, s.errCount = now, res.Replaced, res.Skipped, len(errs)
	if res.Replaced > 0 {
		s.totalRenewed += res.Replaced
		s.lastRenewAt = now
	}
	// 🛑 lastErr を空に戻さない。失敗のあとに「対象なし」の周回が来ただけで
	// 直近のエラーが消えると、守りが切れる日でも受入が緑になる。
	if len(errs) > 0 {
		s.totalErrors += len(errs)
		s.lastErr = errs[0].Error()
		s.lastErrAt = now
	}
}

// view は API に出す形。🛑 **一度も走っていない**(場外だけで起動した等)を
// 「エラーなし = 健全」と読ませない。
func (s *protectiveReplaceState) view() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ranAt.IsZero() {
		return map[string]any{
			"ran":           false,
			"renewed_total": 0,
			"errors_total":  0,
			"note": "守りの置き直し(取消 → 再発注)はまだ一度も走っていない。" +
				"走るのは**寄り前の窓(07:00〜09:00)の営業日だけ**なので、その外で起動した" +
				"場合は false が正常。**エラーが無いことを健全と読まない**",
		}
	}
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(clock.JST).Format("2006-01-02 15:04:05")
	}
	return map[string]any{
		"ran":    true,
		"ran_at": stamp(s.ranAt),
		// 最後の周回ぶん
		"renewed": s.renewed,
		// 🛑 skipped があるから `renewed: 0` の意味が割れる。「期日が近いものが
		// 無くて何もしなかった」と「対象はあったが全部失敗した」は別の状態。
		"skipped": s.skipped,
		"errors":  s.errCount,
		// 通算 — **置き直しが実機で通ったかはこちらで判断する**。
		// renewed_total > 0 が「立花が置き直しを受け付けた」の唯一の証拠。
		// 🛑 最後の周回だけを見ると、成功した直後の周回で 0 に戻って消える。
		"renewed_total": s.totalRenewed,
		"last_renew_at": stamp(s.lastRenewAt),
		"errors_total":  s.totalErrors,
		"last_error":    s.lastErr,
		"last_error_at": stamp(s.lastErrAt),
	}
}

// lastDailyClose は値幅制限の基準値段(前日終値)を日足の最終足から引く。
// ⚠ `priceLimitRef`(trading_cycle.go)と同じ定義 —— 保存済み日足の最後の Close。
// 読めない銘柄は 0 を返す = 「判定できない」で、呼び手は**何も変えない**に倒す。
func lastDailyClose(candles port.CandleRepository) func(context.Context, string) float64 {
	return func(ctx context.Context, symbol string) float64 {
		if candles == nil {
			return 0
		}
		cs, err := candles.List(ctx, symbol, port.PeriodDaily, 1)
		if err != nil || len(cs) == 0 {
			return 0
		}
		return cs[len(cs)-1].Close
	}
}
