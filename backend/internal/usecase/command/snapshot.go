package command

import (
	"context"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// SnapshotCaps carries bot_config / hard_limits values as plain scalars — the
// usecase layer must not import config types (R1).
type SnapshotCaps struct {
	MaxDailyLossJPY              int
	MaxConsecutiveLosses         int
	PerSymbolMaxOpenPositions    int
	AccountMaxOpenPositions      int
	AccountMaxDailyLossJPY       int
	DisableConsecutiveLossGuards bool
	RequiredMarginRate           float64
	// レバレッジ上限(建玉合計 ≤ 保証金 × 倍率)。0 = 無効(research は対象外)。
	// 保証金は broker の Equity を使う — 「持っている額」であって「建てられる額」
	// (買付余力 = 保証金 ÷ required_rate)ではない。
	MaxGrossNotionalRatio float64
	// MinCollateralJPY は最低委託保証金(hard_limits `margin.min_collateral_jpy`)。
	// 保証金(レバ上限と同じ Equity)がこれを割ったら新規 entry を断る。0 = 無効。
	MinCollateralJPY int
	// MaxRiskPerTradeJPY は 1本あたりの計画損失(SL幅×株数)の上限。0 = 無効。
	// 🛑 **構造側の cap**。判定材料は Signal だけなので broker を触らずに落とせる。
	MaxRiskPerTradeJPY int
	WindowMinutes      int
	// AccountMaxEntriesPerDay は口座全体の「1 営業日の新規本数」の上限。0 = 無効(research)。
	// 数は台帳から数える(再起動しても同じ)。0 のときは数えない = 毎ティックの往復を足さない。
	AccountMaxEntriesPerDay int

	// AccountMaxOpenSymbols / EntryArmMaxOpenSymbols は **監視銘柄の予算**。
	// 0 = 無効(live / backtest は使わない)。
	//
	// 🚨 本数の cap とは目的が違う: 時価は 1 リクエストに 120 銘柄まで積め、
	// `BatchQuoteFeed` は 121 銘柄目で間隔を伸ばして通信量を一定に保つ。つまり
	// **API 回数は増えず、時価の実効間隔が 3秒 → 6秒 → 9秒 と落ちる**。監視集合は
	// 3 トラック共有なので、paper の建玉が live の決済判定まで粗くする。
	AccountMaxOpenSymbols  int
	EntryArmMaxOpenSymbols int
}

// SnapshotBuilder assembles the risk.AccountSnapshot the gate evaluates.
type SnapshotBuilder struct {
	posRepo   port.PositionRepository
	tradeRepo port.TradeRepository
	broker    port.Broker
	emergency EmergencyController
	hours     session.TradingHours
	clock     clock.Clock
	caps      SnapshotCaps
	cooldown  CooldownPolicy
	// margin は口座の保証金照会。**必ず非 nil**(コンストラクタが専用の物を作る)。
	// 直接 broker を呼ばないのは、entry 経路が銘柄ごと・ティックごとに wire を
	// 打つ事故を型で塞ぐため。詳細は AccountMarginCache。
	margin *AccountMarginCache
	// blocks は live の銘柄ごとの新規停止(人間のボタン)。nil = 配線しない track(research)。
	blocks port.SymbolBlockReader
}

// WithSymbolBlocks は銘柄ごとの新規停止を snapshot に載せる。**live だけ**に配線する。
// 読めない・壊れているときは SymbolBlocksUnreadable を立て、その track の新規を全部止める。
func (s *SnapshotBuilder) WithSymbolBlocks(r port.SymbolBlockReader) *SnapshotBuilder {
	s.blocks = r
	return s
}

// CooldownPolicy は決済後に同じ銘柄を再エントリーできるまでの待ち時間(秒)。
// 0 = そのクールダウン無効。損切りと利確を分けるのは、負けは「セットアップが
// 壊れた」合図なのに対し、勝ちは壊れていないため。
type CooldownPolicy struct {
	AfterLossSeconds       int
	AfterTakeProfitSeconds int
}

func (s *SnapshotBuilder) WithCooldown(p CooldownPolicy) *SnapshotBuilder {
	s.cooldown = p
	return s
}

// defaultAccountMarginMaxAge は **共有キャッシュを挿し忘れたとき**の上限。
// 0(= 毎回 wire)にしないのは、配線漏れが銘柄ごとの照会と同じ通信量に静かに戻る形
// だから — 忘れても 1 分に 1 回までしか飛ばない。production は WithAccountMargin で
// トラック共有の物に差し替える(そちらは約定 / 決済で無効化される)。
const defaultAccountMarginMaxAge = time.Minute

func NewSnapshotBuilder(pr port.PositionRepository, tr port.TradeRepository, b port.Broker, em EmergencyController, hours session.TradingHours, c clock.Clock, caps SnapshotCaps) *SnapshotBuilder {
	if c == nil {
		c = clock.System()
	}
	return &SnapshotBuilder{posRepo: pr, tradeRepo: tr, broker: b, emergency: em, hours: hours, clock: c, caps: caps,
		margin: NewAccountMarginCache(b, defaultAccountMarginMaxAge, c)}
}

// WithAccountMargin はトラック共有の口座照会キャッシュを挿す。
//
// 🛑 **口座単位の量なので、共有しないと意味が半分になる**: 銘柄ごとに別のキャッシュを
// 持つと「銘柄数 × 更新回数」だけ wire が飛ぶし、A 銘柄の約定で B 銘柄のキャッシュが
// 無効化されない。production は必ずこちらを使う。
func (s *SnapshotBuilder) WithAccountMargin(c *AccountMarginCache) *SnapshotBuilder {
	if c != nil {
		s.margin = c
	}
	return s
}

// BuildStructural assembles everything the gate can judge WITHOUT touching the
// broker — 我々の帳簿(建玉・約定履歴)と時計だけ。broker は 1 回も呼ばない。
//
// 非エントリー sig も受ける(その場合エントリー固有の担保フィールドは 0 のまま)。
// execKind は**解決済みの執行区分**(config ではなく TradingCycle が holding mode から
// 決めた値)。信用注文の collateral 判定に信用の建余力を使うために要る。
//
// 🛑 「BuildStructural + FillCollateral をまとめて呼ぶ Build」は**置かない**。
// 呼ぶだけで口座照会を払う入口があると、いずれ誰かが構造ゲートの前でそれを呼ぶ —
// 予算を焼き切るのは、まさにその形の 1 行。
//
// 🛑 返り値は **MarginStatusUnknown=true**。「まだ訊いていない」を「充分」と取り違えた
// 瞬間に担保チェックが素通りするので、未照会はゼロ値ではなく明示的に不明へ倒す。
// この snapshot を誤って risk.EvaluateSignal に渡しても margin_status_unavailable で
// reject される — 配線ミスが fail-open にならないのはこの 1 行による。
func (s *SnapshotBuilder) BuildStructural(ctx context.Context, symbol string, summary *market.MarketSummary, sig strategy.Signal, execKind order.ExecKind) risk.AccountSnapshot {
	now := s.clock()
	startOfDay := s.startOfTradingDay(now)
	windowSince := now.Add(-time.Duration(s.windowMinutes()) * time.Minute)

	snap := risk.AccountSnapshot{
		EmergencyStop:                 s.emergency != nil && s.emergency.Active(),
		Now:                           now,
		MaxDailyLossJPY:               s.caps.MaxDailyLossJPY,
		MaxConsecutiveLosses:          s.caps.MaxConsecutiveLosses,
		AccountMaxOpenPositions:       s.caps.AccountMaxOpenPositions,
		AccountMaxOpenSymbols:         s.caps.AccountMaxOpenSymbols,
		EntryArmMaxOpenSymbols:        s.caps.EntryArmMaxOpenSymbols,
		AccountMaxDailyLossJPY:        s.caps.AccountMaxDailyLossJPY,
		MaxRiskPerTradeJPY:            s.caps.MaxRiskPerTradeJPY,
		AccountMaxEntriesPerDay:       s.caps.AccountMaxEntriesPerDay,
		ConsecutiveLossGuardsDisabled: s.caps.DisableConsecutiveLossGuards,
		OutsideSessionHours:           !s.hours.InTradingHours(now),
		EndOfDayCloseRequired:         s.hours.IsAfterEntryCutoff(now),
	}

	if s.blocks != nil {
		blocks, err := s.blocks.List(ctx)
		snap.SymbolBlocksUnreadable = err != nil
		snap.ManualSymbolBlocked = err == nil && port.IsSymbolBlocked(blocks, symbol)
	}

	// Every repo read below feeds a never-overridable gate (nanpin, daily-loss,
	// count/window caps), so a read failure must FAIL CLOSE: any single failure
	// taints the whole snapshot and the gate rejects, rather than evaluating those
	// gates against a silent zero.
	failClose := func(err error) bool {
		if err != nil {
			snap.RepoStatusUnknown = true
			return true
		}
		return false
	}

	// 決済直後の再エントリー抑止。無いと損切りした次の秒に同じ銘柄を買い直す
	// (実測: 6976 が 47 秒保有で損切り → 同じ秒に再エントリー)。
	// スリッページを払い続けるうえ、相関した再エントリーが独立標本として数えられて
	// audition の統計を歪める。
	if s.tradeRepo != nil && (s.cooldown.AfterLossSeconds > 0 || s.cooldown.AfterTakeProfitSeconds > 0) {
		last, err := s.tradeRepo.LastCloseBySymbol(ctx, symbol)
		if failClose(err) {
			last = nil
		}
		if last != nil {
			secs, kind := s.cooldown.AfterTakeProfitSeconds, "after_take_profit"
			if last.NetJPY < 0 {
				secs, kind = s.cooldown.AfterLossSeconds, "after_loss"
			}
			if secs > 0 {
				until := last.ClosedAt.Add(time.Duration(secs) * time.Second)
				if now.Before(until) {
					snap.InCooldown = true
					snap.CooldownUntil = until
					snap.CooldownKind = kind
				}
			}
		}
	}

	positions, err := s.posRepo.ListOpenOrClosing(ctx, symbol)
	failClose(err)
	// 監視銘柄の予算は「この銘柄が**既に監視集合に居るか**」で枠の消費が決まる。
	// external も数える — 人間の建玉でも時価は引くので通信コストは同じ。
	snap.SymbolAlreadyHeld = len(positions) > 0
	// 入口(兄弟アーム込み)がこの銘柄を持っているか。🛑 **戦略名の完全一致だけ**を見る
	// (ナンピン判定の「不明は全戦略に数える」とは逆に倒す): ここで曖昧な建玉を
	// 「自分の入口が持っている」と読むと、入口の予約枠を素通しできてしまう。
	arms := strategy.SiblingArms(sig.StrategyName)
	for _, p := range positions {
		for _, a := range arms {
			if p.StrategyName == a {
				snap.EntryArmHoldsSymbol = true
			}
		}
	}
	for _, p := range positions {
		sameStrategyBot := p.StrategyName == "" || p.StrategyName == string(sig.StrategyName)
		if p.Source != position.SourceExternal {
			snap.OpenPositions++ // per-symbol capacity excludes external
			// 建玉枠のキーも (銘柄, 戦略) へ。ここを埋め忘れると paper の枠が
			// **常に 0** になり、同一戦略の積み増しが素通りする(fail-open)。
			// 🛑 戦略が分からない旧建玉は全戦略に数える(ナンピン判定と同じ倒し方)。
			if sameStrategyBot {
				snap.OpenPositionsSameStrategy++
			}
		}
		// ナンピン禁止のキーが (銘柄, 側, 戦略) なので、同一戦略の本数も数える。
		// 🛑 **戦略が分からない建玉は全戦略に対して数える**(fail-close): external
		// (人間が証券アプリで建てた)と、strategy_name が空の旧建玉
		// (migration 0015 の backfill が届かなかった行)。「戦略が違う」と読むと、
		// 人間の建玉や旧建玉と同じ銘柄・同じ側を二重に持つ。
		sameStrategy := p.Source == position.SourceExternal ||
			p.StrategyName == "" || p.StrategyName == string(sig.StrategyName)
		switch p.Side {
		case order.SideBuy:
			snap.OpenBuyInclExternal++
			if sameStrategy {
				snap.OpenBuySameStrategyInclExternal++
			}
		case order.SideSell:
			snap.OpenSellInclExternal++
			if sameStrategy {
				snap.OpenSellSameStrategyInclExternal++
			}
		}
	}
	// 🛑 本数・銘柄数・入口の銘柄数を **1 クエリ**で取る。この集計は armed 銘柄の
	// シグナルごと(場中は 3〜6 秒おき)に走るので、3 往復に割らない。
	counts, e := s.posRepo.CountOpenAcross(ctx, arms)
	failClose(e)
	snap.AccountOpenPositions = counts.Positions
	snap.AccountOpenSymbols = counts.Symbols
	snap.EntryArmOpenSymbols = counts.EntryArmSymbols
	snap.DailyLossJPY, e = s.tradeRepo.SumClosedLossJPYSinceBySymbol(ctx, symbol, startOfDay)
	failClose(e)
	snap.AccountDailyLossJPY, e = s.tradeRepo.SumClosedLossJPYSince(ctx, startOfDay)
	failClose(e)
	if s.caps.AccountMaxEntriesPerDay > 0 {
		snap.AccountEntriesToday, e = s.posRepo.CountOpenedSince(ctx, startOfDay)
		failClose(e)
	}
	snap.ConsecutiveLosses, e = s.tradeRepo.ConsecutiveLossesBySymbol(ctx, symbol)
	failClose(e)
	snap.TradesInWindow, e = s.tradeRepo.CountTradesSinceBySymbol(ctx, symbol, windowSince)
	failClose(e)
	// 日中保有の「同日 1 回転」。日中の建てでだけ訊く(多日の毎ティックに
	// 往復を足さない)。読めなければ他の枠と同じく fail-close。
	if sig.HoldingMode == order.HoldingIntraday {
		snap.EntriesTodaySameStrategy, e = s.posRepo.CountOpenedSinceBySymbolStrategy(ctx, symbol, string(sig.StrategyName), startOfDay)
		failClose(e)
	}
	snap.LossInWindowJPY, e = s.tradeRepo.SumClosedLossJPYSinceBySymbol(ctx, symbol, windowSince)
	failClose(e)

	if sig.IsEntry() && s.caps.RequiredMarginRate > 0 {
		// 必要担保は sig だけで決まる純粋計算。broker には依らないのでここで置ける。
		snap.CollateralRequiredJPY = int(sig.EntryPrice * float64(sig.Quantity) * s.caps.RequiredMarginRate)
	}
	// 未照会 = 不明。FillCollateral が照会に成功したときだけ降りる。
	snap.MarginStatusUnknown = true
	return snap
}

// FillCollateral は担保 / レバレッジの入力を埋める。値は **AccountMarginCache 経由**
// で取る(立花では 1 回の照会 = wire 3 リクエスト)。
//
// 🚨 **ここが wire を打つのは、キャッシュが空か期限切れのときだけ。**
// ティックごと・銘柄ごとに打つと、`gross_notional_cap` で必ず落ちる armed 銘柄
// 1 本が後場だけで wire を数千回焼く。保証金は口座単位の量なので、更新は
// 「初回 / 約定・決済 / maxAge 経過」だけでよい(AccountMarginCache)。
//
// 構造ゲートを通ったエントリーについてだけ呼ぶこと。照会に失敗したときは
// MarginStatusUnknown を立てたまま返し、担保ゲートが fail-close する。
func (s *SnapshotBuilder) FillCollateral(ctx context.Context, snap *risk.AccountSnapshot, sig strategy.Signal, execKind order.ExecKind) {
	// FAIL-CLOSE: an unreportable margin rejects entries rather than silently
	// skipping the collateral gate.
	if am, ok := s.margin.Get(ctx); ok && am != nil {
		snap.MarginStatusUnknown = false
		snap.MarginRatio = am.MarginRatio
		snap.CollateralJPY = int(am.Equity)
		snap.MinCollateralJPY = s.caps.MinCollateralJPY
		// 🛑 **信用注文は信用の建余力で判定する**。現物の買付余力で見ていたため、
		// 信用で建てられない口座でも事前ガードが素通りする
		// (「新規建余力は0円です。(最低保証金割れ)」を立花の拒否でしか止められない)。
		if execKind.IsMargin() {
			snap.AvailableToTradeJPY = int(am.MarginNewJPY)
			// 追証は維持率 trip とは別軸の停止条件。建て増しを許さない。
			if am.MarginCall {
				snap.AvailableToTradeJPY = 0
			}
		} else {
			snap.AvailableToTradeJPY = int(am.AvailableJPY)
		}
	}
	// レバ上限の入力。**有効なときだけ**建玉を引く(research では 200銘柄ぶんの
	// 追加クエリを毎ティック走らせない)。読めなければ 0 のまま = fail-close 側。
	if s.caps.MaxGrossNotionalRatio > 0 {
		snap.MaxGrossNotionalRatio = s.caps.MaxGrossNotionalRatio
		open, err := s.posRepo.ListOpenAllSymbols(ctx)
		if err != nil {
			snap.RepoStatusUnknown = true
		} else {
			snap.OpenGrossNotionalJPY = 0
			for _, p := range open {
				snap.OpenGrossNotionalJPY += int(p.EntryPrice * float64(p.Quantity))
			}
		}
	}
}

func (s *SnapshotBuilder) windowMinutes() int {
	if s.caps.WindowMinutes > 0 {
		return s.caps.WindowMinutes
	}
	return 60
}

// startOfTradingDay is midnight in the VENUE timezone (JST), not UTC.
func (s *SnapshotBuilder) startOfTradingDay(now time.Time) time.Time {
	return s.hours.DayStart(now)
}
