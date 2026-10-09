package main

import (
	"log/slog"
	"time"

	"stockbot/backend/internal/app"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
	"stockbot/backend/internal/usecase/command"
)

// wiringDeps are the shared singletons every symbol bundle is built from.
type wiringDeps struct {
	broker     port.LiveBroker
	posRepo    port.PositionRepository
	tradeRepo  port.TradeRepository
	closer     port.PositionCloser
	candles    port.CandleRepository
	rejections port.SignalRejectionRepository
	pending    port.PendingPositionTracker
	emergency  *safety.EmergencyStop
	engine     *strategy.Engine
	hours      session.TradingHours
	clock      clock.Clock
	botCfg     *config.BotConfig
	hardLimits *config.HardLimits
	counters   *app.Counters
	logger     *slog.Logger

	// marginCache は **このトラック共有**の口座照会キャッシュ(accountMargin() が遅延生成)。
	// 🛑 直接書かない — トラックを足したときに初期化を忘れると、そのトラックだけ
	// 銘柄ごとのキャッシュに落ちて、銘柄ごとに照会する通信量に戻る。
	marginCache *command.AccountMarginCache

	// split は**このトラック共有**の株式分割ガード(splitGuard() が遅延生成)。
	split *command.SplitGuard

	// entrySerial は**このトラック共有**の entry 排他(entryLock() が遅延生成・S2)。
	entrySerial *command.EntrySerializer

	// symbolBlocks は人間の銘柄ごとの新規停止。**live だけ**が挿す(research は nil = 止まらない)。
	symbolBlocks port.SymbolBlockReader
}

// entryLockWait は口座単位の entry 排他を待つ上限。待っている間はその銘柄の price
// goroutine が止まる(= 次のティックの決済判定が遅れる)ので、立花の約定待ち(最大 15 秒)
// より短くしてある。待ちきれなければ見送って次のティックで取り直す。paper の区間は
// ミリ秒なので、ここで落ちるのは live の往復と重なったときだけ。
const entryLockWait = 10 * time.Second

// entryLock は**トラック(= 口座)に 1 つ**の entry 排他。口座の枠は
// snapshot の件数で判定するので、銘柄ごとの goroutine が同時に最後の 1 枠を見ると両方建つ。
func (d *wiringDeps) entryLock() *command.EntrySerializer {
	if d.entrySerial == nil {
		d.entrySerial = command.NewEntrySerializer(entryLockWait)
	}
	return d.entrySerial
}

// splitGuard は株式分割の権利落ちから建玉を守るガード。
//
// 🛑 **live は必ず broker の建玉照会で確かめる**(SplitAuthorityBroker)。mode で決める —
// broker の型で決めると、ラッパの変更ひとつで live が値段の証拠だけで台帳を書き換える側へ
// 黙って落ちる。paper は値段の証拠で言い直し、紙の帳簿も揃える。
func (d *wiringDeps) splitGuard() *command.SplitGuard {
	if d.split != nil {
		return d.split
	}
	authority := command.SplitAuthorityPrice
	if d.botCfg != nil && d.botCfg.Mode == config.ModeLive {
		authority = command.SplitAuthorityBroker
	}
	// nil のインタフェースを渡さない(型付き nil は EmergencyController の nil 判定を素通りする)。
	var em command.EmergencyController
	if d.emergency != nil {
		em = d.emergency
	}
	g := command.NewSplitGuard(d.posRepo, d.candles, d.broker, d.hours, em, authority)
	if d.logger != nil {
		logger := d.logger
		g = g.WithLogger(func(msg string, kv ...any) { logger.Warn(msg, kv...) })
	}
	if authority == command.SplitAuthorityPrice {
		if book, ok := d.broker.(port.SplitAdjustableBook); ok {
			g = g.WithPaperBook(book)
		}
	}
	d.split = g
	return g
}

// accountMarginMaxAge は口座照会の値を持ち回れる上限。維持率ブレーカーの周期
// (marginHeldInterval)と同じ 1 時間 —— 保有中はブレーカーの実照会が必ず先に
// 温めるので、この期限で追加の wire が飛ぶのは「無保有のまま 1 時間」だけ。
const accountMarginMaxAge = time.Hour

// accountMargin は **トラックに 1 つ**の口座照会キャッシュ。保証金は口座単位の量なので、
// 銘柄ごとに持つと(a) 銘柄数ぶん wire が飛び (b) A 銘柄の約定で B 銘柄の値が
// 古いまま残る。更新は「初回 / 約定・決済 / 1時間」だけ(AccountMarginCache)。
func (d *wiringDeps) accountMargin() *command.AccountMarginCache {
	if d.marginCache == nil {
		d.marginCache = command.NewAccountMarginCache(d.broker, accountMarginMaxAge, d.clock)
	}
	return d.marginCache
}

// buildSymbolBundle assembles the per-symbol bundle from the shared deps.
func buildSymbolBundle(d *wiringDeps, symbol string, active *config.StrategyConfig) *app.SymbolBundle {
	caps := command.SnapshotCaps{
		MaxDailyLossJPY:           d.botCfg.Risk.MaxDailyLossJPY,
		MaxConsecutiveLosses:      d.botCfg.Risk.MaxConsecutiveLosses,
		PerSymbolMaxOpenPositions: d.botCfg.Risk.MaxOpenPositions,
		AccountMaxOpenPositions:   d.botCfg.Risk.AccountMaxOpenPositions,
		// 監視銘柄の予算。0 = 無効で、live config は持てない。
		AccountMaxOpenSymbols:        d.botCfg.Risk.AccountMaxOpenSymbols,
		EntryArmMaxOpenSymbols:       d.botCfg.Risk.PerEntryMaxOpenSymbols,
		AccountMaxDailyLossJPY:       d.botCfg.Risk.AccountMaxDailyLossJPY,
		DisableConsecutiveLossGuards: d.botCfg.Risk.DisableConsecutiveLossGuards,
		RequiredMarginRate:           d.hardLimits.Margin.RequiredRate,
		// 最低委託保証金(D-2)。paper の紙の残高(hard_limits `paper.balance_jpy`)は桁違いに
		// 大きいので research には効かない。
		MinCollateralJPY:      d.hardLimits.Margin.MinCollateralJPY,
		MaxGrossNotionalRatio: d.botCfg.Risk.MaxGrossNotionalRatio,
		MaxRiskPerTradeJPY:    d.botCfg.Risk.MaxRiskPerTradeJPY,
		// 口座全体の 1 営業日の新規本数。0 = 無効で、research の bot_config は持たない。
		AccountMaxEntriesPerDay: d.botCfg.Risk.AccountMaxEntriesPerDay,
		WindowMinutes:           60,
	}
	snap := command.NewSnapshotBuilder(d.posRepo, d.tradeRepo, d.broker, d.emergency, d.hours, d.clock, caps).
		// 🚨 トラック共有の口座照会キャッシュ。挿さないと entry 判定が銘柄ごと・
		// ティックごとに立花を叩く(後場だけで wire 数千回)。
		WithAccountMargin(d.accountMargin()).
		// 未配線だった間は、損切りした次の秒に買い直すチャーンが起きていた。
		WithCooldown(command.CooldownPolicy{
			AfterLossSeconds:       d.hardLimits.Cooldown.AfterLossSeconds,
			AfterTakeProfitSeconds: d.hardLimits.Cooldown.AfterTakeProfitSeconds,
		})
	if d.symbolBlocks != nil {
		// 止めた銘柄の新規を `manual_symbol_block` で落とす。読めなければ全部落とす(fail-close)。
		snap = snap.WithSymbolBlocks(d.symbolBlocks)
	}
	// WithCloser: 約定した後に守りを置けず巻き戻した往復を **台帳に残す**
	// (close_reason='entry_compensated')。無いと cooldown / 窓の取引回数 / 日次損失 /
	// 連敗が揃って盲目になる(実弾で同じ銘柄を何往復もする原因)。
	exec := command.NewExecuteOrder(d.broker, d.posRepo, d.pending, d.emergency, d.clock).
		WithCloser(d.closer)
	exec.Ops = d.counters

	execKindFor := func(m order.HoldingMode) order.ExecKind { return d.botCfg.ExecKindFor(m) }
	cycle := command.NewTradingCycle(d.engine, snap, exec, d.hardLimits.Quantity.Min, execKindFor, d.hours)
	cycle.Boundary = risk.OrderBoundary{ // fat-finger backstop from hard_limits
		MaxStopLossPct:     d.hardLimits.OrderBoundary.MaxStopLossPct,
		MaxTakeProfitPct:   d.hardLimits.OrderBoundary.MaxTakeProfitPct,
		MaxLossPerTradeJPY: d.hardLimits.OrderBoundary.MaxLossPerTradeJPY,
	}
	cycle.Rejections = d.rejections // "なぜエントリーしなかったか" の永続 audit trail
	// 口座の枠を「見て、取る」区間を口座単位で直列化する。決済と守りは対象外。
	cycle.EntryLock = d.entryLock()
	// 制度信用の売建は貸借銘柄のみ。一覧が空なら売りは全 reject(fail-close)で、
	// 理由は signal_rejections に残る = 「売り側がゼロなのは一覧が無いから」と分かる。
	// 🛑 `Configured` を**一覧の有無**で立てる。ここをメソッド値だけで配線すると
	// 常に「一覧はある」ことになり、文書が約束する `loanable_list_missing` が
	// production では一度も記録されない。
	cycle.Loanable = risk.LoanableSymbols{
		Configured: len(d.hardLimits.LoanableSymbols) > 0,
		Allows:     d.hardLimits.AllowsShortSymbol,
	}
	// 信用建玉の資金コスト(買方金利 / 貸株料)。net = gross − 手数料 + carry。
	// 料率が hard_limits に無ければ carry は 0(レートを捏造しない)。
	carry := d.hardLimits.CarryCalc(d.hours)
	manage := command.NewManageOpenPositions(d.broker, d.posRepo, d.closer, d.emergency, d.clock, d.hardLimits.Margin.MaintenanceRatio).
		// ブレーカーの照会は**間引かない**が、その結果は entry 経路へも配る(タダの相乗り)。
		WithAccountMargin(d.accountMargin()).
		WithCarry(carry).
		// 🛑 株式分割の権利落ちを決済判定より先に見る(分割前の円の SL で含み益を損切りしない)。
		WithSplitGuard(d.splitGuard())
	flatten := command.NewForceFlatten(d.broker, d.posRepo, d.closer, d.emergency, d.hours, d.clock).
		WithCarry(carry)
	// 🛑 reconcile にも **同じ carry** を渡す。broker 側の守りが約定して台帳を
	// reconcile が締める往復(多日建玉では普通に起きる)で carry が 0 になると、
	// 同じ出口が経路によって別の net を持つ。
	reconcile := command.NewReconcile(d.broker, d.posRepo, d.closer, d.pending, d.emergency, d.clock).
		WithCarry(carry)
	reconcile.Ops = d.counters

	return &app.SymbolBundle{
		Symbol:    symbol,
		Broker:    d.broker,
		Candles:   d.candles,
		Agg:       market.NewAggregator(symbol, 512),
		Configs:   app.NewConfigSet(active),
		Cycle:     cycle,
		Manage:    manage,
		Flatten:   flatten,
		Reconcile: reconcile,
		Hours:     d.hours,
		Clock:     d.clock,
		Counters:  d.counters,
		// 建玉の有無は口座照会レーンのゲート(HoldsPosition)が読む。**常に**挿す —
		// nil だと「判断不能 = 常に照会」に倒れ、間引きが静かに無効化される。
		PosRepo: d.posRepo,
		// 約定 / 決済を最初に観測するのはここ。そこで entry 経路の担保値を捨てる。
		MarginCache: d.accountMargin(),
		// 履歴フォールバックは立花の推奨時間帯だけに許す。
		AllowHistoryFetch: inHistoryFetchWindow,
		Logger:            d.logger,
	}
}

func strategyNameOrNone(c *config.StrategyConfig) string {
	if c == nil {
		return "none"
	}
	return string(c.StrategyName)
}

// defaultNoTradeConfig is the fail-safe used when no strategy config matches a
// symbol: no_trade never enters.
func defaultNoTradeConfig(symbol string, mode config.Mode) *config.StrategyConfig {
	c := &config.StrategyConfig{
		ConfigID:     "no_trade_default_" + symbol,
		Symbol:       symbol,
		StrategyName: config.StrategyNoTrade,
		Mode:         mode,
		HoldingMode:  config.HoldingIntraday,
	}
	c.Entry.Direction = config.DirectionBoth
	return c
}
