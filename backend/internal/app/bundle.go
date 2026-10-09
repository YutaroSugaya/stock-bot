package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/command"
)

type SymbolBundle struct {
	Symbol  string
	Broker  port.Broker
	Candles port.CandleRepository // 日足の一次ソース。nil 可
	Agg     *market.Aggregator
	// Configs は 1 銘柄に載る active config の**集合**。入口が同一で出口だけが
	// 違う 2 アームを同一ティックで走らせるために、1 config ではなく集合を回す。
	Configs   *ConfigSet
	Cycle     *command.TradingCycle
	Manage    *command.ManageOpenPositions
	Flatten   *command.ForceFlatten
	Reconcile *command.Reconcile
	Hours     session.TradingHours
	Clock     clock.Clock
	Counters  *Counters
	Logger    *slog.Logger

	// 方式B: armed でも建玉中でもない銘柄は broker を poll しない(大きなユニバースでも
	// 毎ティックの API 呼び出しが増えない)。false = 全銘柄 poll。
	MonitorNarrow bool
	PosRepo       port.PositionRepository

	// MarginCache は **トラック共有**の口座照会キャッシュ(nil = 無効)。
	// 🚨 この bundle は約定 / 決済を最初に観測する場所なので、そこで**捨てる**役目を持つ。
	// 維持率ブレーカーの再照会に相乗りさせないのは、あれが `maintenance_ratio<=0` で
	// 丸ごと無効にできる knob だから — 寄せると「リスク設定を 0 にしたら担保の値が
	// 約定後も古いまま」という結び付きが静かに生まれる。
	MarginCache *command.AccountMarginCache

	// broker 日足フォールバック(rawDailyCandles)の時間帯ゲート。あの経路は**毎**
	// price tick で評価されるので、candle repo が一瞬空を返す(pg の一時障害)だけで
	// 1日1回の履歴取得が秒間ポーリングに化ける。立花は履歴取得を閑散時間帯に限るよう
	// 求めている。nil = 常に許可(backtest/テスト配線)。
	AllowHistoryFetch func(time.Time) bool

	// 段階ウォッチ Tier A の共有レジストリ。nil = tiering 無し。この bundle が
	// JST 日ごと最初の場中ティックで自分の判定(前日確定足が BNF パニックか)を書く。
	HotWatch *HotWatch
	hotDay   string // JST day the verdict below was computed for
	hotToday bool

	// 維持率(broker への口座照会)の間引き間隔。**0 = 毎ティック**で、paper /
	// backtest / 既存配線の挙動は 1bit も変わらない。live だけがここに値を入れる。
	//
	// 毎ティック訊いていた頃は live 1銘柄で 13,200回/日 になり、しかも建玉ゼロの間は
	// アダプタが常に定数 1.0 を返す(現物 / flat は健全)ので、その全部が同じ答えを
	// 取り直すだけだった。ローカルで分かることを wire で訊かない。
	MarginPollInterval time.Duration
	marginPolledAt     time.Time
	marginPolledOnce   bool
	marginHeldLast     bool

	mu          sync.RWMutex
	lastSummary *market.MarketSummary
	// 気配のみ(前日終値 fallback = 未約定/売買停止)の最新値。**観測専用** — 取引経路は
	// 読んではいけない。lastSummary と別に持つのは、stale 価格が戦略・risk gate・
	// dashboard の含み損益に事故で届かないようにするため。
	lastIndicative *market.MarketSummary
}

func (b *SymbolBundle) PriceTick(ctx context.Context) {
	now := b.Clock()
	if !b.Hours.InTradingHours(now) {
		return
	}
	// hot 候補は 方式B の narrowing を突破する: 「パニック翌日の分足」は arm の有無と
	// 無関係に記録が要る(hot はパニック翌日限定なので通信量の増加は稀日のみ)。
	hot := b.updateHotWatch(ctx, now)
	if b.MonitorNarrow && !hot && b.notArmedAndFlat(ctx) {
		// 監視を降りた銘柄について「気配のみ」と言い続けない。条件より長生きするバッジは
		// バッジが無いより悪い。
		b.setIndicative(nil)
		return
	}
	ticker, err := b.Broker.GetTicker(ctx, b.Symbol)
	var summary *market.MarketSummary
	healthy := err == nil && !ticker.Stale
	switch {
	case err != nil:
		b.Counters.TickerErrors.Add(1)
		b.Logger.Debug("ticker fetch failed", "symbol", b.Symbol, "err", err)
	case ticker.Stale:
		// 気配のみの値。強制決済の best-effort 評価にしか使わない(集計・エントリー・
		// TP/SL 管理では使わない)。それでも別フィールドで**公開**するのは、非表示にすると
		// 売買停止とフィード故障が区別できなくなるため(実測: 6976 / 6981 /
		// 285A が建玉つきで dashboard から消えた)。
		b.Counters.StaleQuotes.Add(1)
		summary = market.SummaryFromTicker(*ticker, now)
		b.setIndicative(summary)
	default:
		b.Counters.PriceTicks.Add(1)
		b.Agg.OnTick(*ticker, now)
		summary = market.SummaryFromTicker(*ticker, now)
		b.setSummary(summary)
	}

	// 気配の健全性に**関係なく**走らせる守り。かつて「新鮮な気配がある」条件の下に
	// 入れていたが、引け際にフィードが劣化したときこそ発火すべきものだった:
	//   (a) 維持率 breaker — テープではなく broker の証拠金を見る。
	//   (b) 14:50 引け前フラット化 — MARKET 決済。気配が stale/不在でも注文は出す。
	if b.shouldPollMargin(ctx, now) {
		b.Manage.CheckMaintenance(ctx)
	}
	if n, err := b.Flatten.FlattenIfNearClose(ctx, b.Symbol, summary); err != nil {
		b.Logger.Error("force-flatten failed", "symbol", b.Symbol, "err", err)
	} else if n > 0 {
		b.Counters.ForcedFlats.Add(int64(n))
	}

	// ここから先(TP/SL 管理・エントリー)は新鮮な約定可能気配が要る。
	if !healthy {
		return
	}

	if err := b.Manage.OnTick(ctx, b.Symbol, summary); err != nil {
		b.Logger.Warn("manage positions failed", "symbol", b.Symbol, "err", err)
	}

	// 🛑 arm 済みの **全戦略**を回す。1 config しか実行しないと、入口が同一で
	// 出口だけ違う 2 アームのうち片方が永久に建たない = ペアが成立しない。
	// 順序は ConfigSet が戦略名の昇順で固定している(同じ入力 → 同じ発注順)。
	// 1 戦略の失敗は他の戦略を巻き込まない — risk gate の snapshot は Execute ごとに
	// 取り直すので、1 本目の建玉は 2 本目の判定に**ちゃんと効く**(口座枠・ナンピン)。
	for _, cfg := range b.Configs.Active() {
		if cfg == nil {
			continue
		}
		res, err := b.Cycle.Execute(ctx, b.evalInput(ctx, now, summary, cfg))
		if err != nil {
			b.Logger.Error("trading cycle failed", "symbol", b.Symbol, "strategy", string(cfg.StrategyName), "err", err)
			continue
		}
		switch {
		case res.Entered:
			b.Counters.Entries.Add(1)
			b.Logger.Info("entered position", "symbol", b.Symbol, "position_id", res.PositionID, "config", cfg.ConfigID)
		case res.RejectReason != "":
			b.Counters.Rejections.Add(1)
			b.Logger.Debug("entry rejected", "symbol", b.Symbol, "strategy", string(cfg.StrategyName), "reason", res.RejectReason)
		}
	}
}

// 🛑 EvalInput の組み立てを 1 箇所に閉じる。**Hours を落とすと戦略別 MaxHold が
// 祝日を知らない営業日に静かに縮退する** — エラーも警告も出ないので、回帰テストで
// 押さえられるようメソッドに切り出してある(eval_input_test.go)。
func (b *SymbolBundle) evalInput(ctx context.Context, now time.Time, summary *market.MarketSummary, cfg *config.StrategyConfig) strategy.EvalInput {
	return strategy.EvalInput{
		Now:          now,
		Summary:      summary,
		Candles1m:    b.Agg.Candles(time.Minute),
		Candles5m:    b.Agg.Candles(5 * time.Minute),
		Candles1h:    b.Agg.Candles(time.Hour),
		CandlesDaily: b.dailyCandles(ctx),
		Config:       cfg,
		Hours:        b.Hours,
	}
}

// StartupReconcile は価格ループの前に回す同期 reconcile 1 周。
//
// 🚨 **失敗したら、その銘柄の新規 entry を reconcile が 1 回成功するまで止める**。
// 以前は Warn を出して価格ループへ進んでいた = 再起動直後に broker 照会が落ちると、
// 台帳が broker の建玉を知らないままナンピン禁止ゲートを通す。保留は**先に立てて成功で
// 落とす**(照会が panic して戻らなくても fail-close のまま)。起動は止めない —
// 守りの置き直し・決済・reconcile レーンは動かし続け、次の成功で解除される。
func (b *SymbolBundle) StartupReconcile(ctx context.Context) {
	if b.Reconcile == nil || b.Cycle == nil {
		return // 解除する経路が無いので保留しない
	}
	b.Cycle.HoldEntriesUntilReconciled()
	b.ReconcileTick(ctx)
	if b.Cycle.EntriesHeldForReconcile() {
		b.Logger.Warn("startup reconcile failed: new entries held until a reconcile succeeds", "symbol", b.Symbol)
	}
}

// EntriesHeldForReconcile は起動時 reconcile が未成功で新規 entry を保留中か。
func (b *SymbolBundle) EntriesHeldForReconcile() bool {
	return b.Cycle != nil && b.Cycle.EntriesHeldForReconcile()
}

func (b *SymbolBundle) ReconcileTick(ctx context.Context) {
	if b.Reconcile == nil {
		return
	}
	rep, err := b.Reconcile.Run(ctx, b.Symbol)
	if err != nil {
		b.Logger.Warn("reconcile failed", "symbol", b.Symbol, "err", err)
		return
	}
	if b.Cycle != nil {
		b.Cycle.MarkReconciled()
	}
	// 決済を出したのに約定が確認できない建玉は、守りの脚を cancel 済みなので**裸**。
	// trip はしない(板に決済注文が残っているうちは待つのが正しい)が、黙って
	// 積もらせない — 報告を捨てていたので今まで画面にもログにも出ていなかった。
	// external の実額が台帳に残らなかった。監査で復元できないので必ず surface する。
	if rep.ExternalUnbooked > 0 {
		b.Logger.Warn("external position closed at the broker but could not be booked (price or entry price unobservable)",
			"symbol", b.Symbol, "external_unbooked", rep.ExternalUnbooked)
	}
	if rep.StuckClosing > 0 {
		b.Logger.Warn("settle fill unconfirmed, position left CLOSING (protective legs already cancelled = naked)",
			"symbol", b.Symbol, "stuck_closing", rep.StuckClosing)
	}
}

// 5日 = 通常の連休(金の引け → 火/水の寄り)を跨げる幅。これを超えて古ければ
// フィードが凍っているとみなす。**Hours(休場カレンダー)が無いときの fallback。**
const maxDailyCandleAge = 5 * 24 * time.Hour

// 営業日で数える鮮度の許容。最新バーの翌営業日から今日までの
// 営業日数がこれを超えたら凍結。平常は 1(昨日のバー)。3 は暦日 5 日と同じ許容
// (金曜のバーを水曜まで許す = 取りこぼし 2 日)。暦日で数えるとシルバーウィーク明けの
// 9/24 や年始 1/4 に、3 トラック全部の日足戦略が連休明けの 1 日を丸ごと沈黙する。
const maxDailyCandleAgeTradingDays = 3

// dailyStale は最新バーが古すぎるか。休場カレンダーがあれば営業日で、無ければ暦日で数える。
func (b *SymbolBundle) dailyStale(latest, now time.Time) bool {
	if b.Hours.TZ == nil {
		return now.Sub(latest) > maxDailyCandleAge
	}
	today := b.Hours.DayStart(now)
	n := 0
	for d, i := b.Hours.DayStart(latest).AddDate(0, 0, 1), 0; !d.After(today); d, i = d.AddDate(0, 0, 1), i+1 {
		// カレンダーの外は営業日として数える(凍結側に倒す)。IsTradingDay は期限切れで
		// false を返すので、そのまま使うと古いバーを永久に「新鮮」と読む。
		if b.Hours.IsTradingDay(d) || !b.Hours.CalendarCovers(d) {
			n++
		}
		if n > maxDailyCandleAgeTradingDays || i > 60 {
			return true
		}
	}
	return false
}

// 鮮度ガード付き: 最新バーが maxDailyCandleAge より古ければ nil を返す。戦略は
// insufficient_daily_history でスキップし、**今日の値段を古いバーと突き合わせない**。
func (b *SymbolBundle) dailyCandles(ctx context.Context) []market.Candle {
	cs := b.rawDailyCandles(ctx)
	if len(cs) == 0 {
		return nil
	}
	latest := cs[0].OpenTime
	for _, c := range cs {
		if c.OpenTime.After(latest) {
			latest = c.OpenTime
		}
	}
	if b.Clock != nil && b.dailyStale(latest, b.Clock()) {
		if b.Counters != nil {
			b.Counters.StaleDaily.Add(1)
		}
		if b.Logger != nil {
			b.Logger.Warn("daily candles stale; skipping day-horizon eval", "symbol", b.Symbol, "latest_bar", latest)
		}
		return nil
	}
	return cs
}

func (b *SymbolBundle) rawDailyCandles(ctx context.Context) []market.Candle {
	if b.Candles != nil {
		if cs, err := b.Candles.List(ctx, b.Symbol, port.PeriodDaily, strategy.DailyBarsRequired); err == nil && len(cs) > 0 {
			// 🛑 **repo から読んだ日足にも段差ガードを掛ける**。broker 直読み経路
			// (下)には元から掛かっていたのに、通常使うこちらには無かった。DB に
			// 未調整の分割が残っていると 25日線が実勢の 3〜4 倍になり、乖離が
			// -65〜-84% の偽のパニックに化ける。**逆張り(BNF)はそれに構造的に
			// 引き寄せられる** — 実弾トラックが分割未調整の銘柄を arm する経路。
			// 取り込みが正しいことを願うのではなく、壊れたデータを使わせない。
			if d := market.SplitDiscontinuity(cs); d != "" {
				if b.Counters != nil {
					b.Counters.StaleDaily.Add(1)
				}
				if b.Logger != nil {
					b.Logger.Warn("stored daily bars contain a split-sized discontinuity — not evaluating day-horizon; run `make fetch-daily` to chain-link",
						"symbol", b.Symbol, "detail", d)
				}
				return nil
			}
			return cs
		}
	}
	// 毎ティック評価される経路なので履歴取得の時間帯ゲートを掛ける(掛けないと pg の
	// 一時障害が秒間の履歴ポーリングに化ける)。窓の外は nil = 戦略はスキップ。
	if b.AllowHistoryFetch != nil && b.Clock != nil && !b.AllowHistoryFetch(b.Clock()) {
		return nil
	}
	// ⚠ **立花の日足は 250 本が上限**なので、この fallback に落ちた銘柄では
	// `high_52w_momentum`(要求 253 本)は依然として評価されない。repo(CSV/DB)経路が
	// 生きているときだけ動く — 修正は「全部動く」ではない。上限を上げられない
	// のは broker 側の制約なので、要求値ではなく broker の上限をそのまま書く。
	const brokerMaxDailyBars = 250
	cs, err := b.Broker.GetKlines(ctx, b.Symbol, port.PeriodDaily, brokerMaxDailyBars)
	if err != nil {
		return nil
	}
	// broker(立花)の日足は**分割未調整**: 1:5 分割は見かけ −80% のバーになり、
	// 逆張り(BNF)にとって最良のエントリー条件に化ける。repo 経由の取り込み側だけ
	// 弾いてもこの直読み経路が残るとガードが半分になる。疑わしい系列は「日足なし」扱い。
	if d := market.SplitDiscontinuity(cs); d != "" {
		if b.Logger != nil {
			b.Logger.Warn("broker daily klines look split-unadjusted — not evaluating day-horizon",
				"symbol", b.Symbol, "detail", d)
		}
		return nil
	}
	return cs
}

// bundle 自身の price goroutine からしか呼ばれないので、日次キャッシュ欄に lock は
// 要らない(共有レジストリ側は読み手のために lock 済み)。
func (b *SymbolBundle) updateHotWatch(ctx context.Context, now time.Time) bool {
	if b.HotWatch == nil {
		return false
	}
	tz := b.Hours.TZ
	if tz == nil {
		tz = time.UTC
	}
	day := now.In(tz).Format("2006-01-02")
	if b.hotDay == day {
		return b.hotToday
	}
	b.hotDay = day
	// 日足が凍っていれば nil → cold のまま(通信量を増やす側に倒さない)。
	b.hotToday = strategy.IntradayHotCandidate(b.Symbol, b.dailyCandles(ctx), now)
	b.HotWatch.Set(b.Symbol, b.hotToday)
	if b.hotToday && b.Logger != nil {
		b.Logger.Info("tier_a_hot_candidate", "symbol", b.Symbol, "day", day)
	}
	return b.hotToday
}

// shouldPollMargin decides whether this tick may spend a broker 口座照会 on the
// 維持率 breaker. 呼ばれるのは PriceTick の中だけ = 場中に限られる。
//
// 訊く条件:
//   - 起動後の 1 回目(= 朝の同期)
//   - 建玉の有無が変わったとき(**約定直後 / 決済直後**の再同期)
//   - 保有中の MarginPollInterval ごと
//
// 訊かない条件は「答えが分かっている」ときだけに限る: 建玉ゼロなら維持率は定数 1.0
// (現物 / flat は健全)で、ブレーカーは 0 < ratio < 閾値 でしか落ちない。
//
// 🛑 price goroutine 専用(hotDay と同じ)。他から呼ぶなら lock が要る。
func (b *SymbolBundle) shouldPollMargin(ctx context.Context, now time.Time) bool {
	if b.MarginPollInterval <= 0 {
		return true // 間引き無効 = 現行どおり毎ティック
	}
	held := b.HoldsPosition(ctx)
	changed := held != b.marginHeldLast
	b.marginHeldLast = held
	if changed {
		// 🚨 約定 / 決済で口座は変わった。entry 経路が読む値をここで捨てる
		// (nil-safe)。次に担保を要る判定が来たとき 1 回だけ訊き直される。
		b.MarginCache.Invalidate()
	}

	switch {
	case !b.marginPolledOnce: // 朝(起動後)の 1 回
	case changed: // 建玉が入った / 決済された
	case !held: // 無保有 — 訊く理由が無い
		return false
	case now.Sub(b.marginPolledAt) < b.MarginPollInterval:
		return false
	}
	b.marginPolledOnce = true
	b.marginPolledAt = now
	return true
}

// HoldsPosition reports whether this symbol has an OPEN/CLOSING position, so the
// caller can skip account polling while there is nothing to ask about.
//
// 🛑 判断が付かないとき(repo 未配線 / 読めない)は **true**。口座照会の間引きは
// 「訊く理由が無い」ことに依存しているので、分からないまま無保有に倒すと、通信量の
// 節約が守りの節約に化ける。倒すなら必ず照会する側へ。
// IsArmed は「no_trade 以外の戦略が入っている」= 発注を試みうる銘柄。
// reconcile の照会条件に使う: 台帳に建玉が無くても、arm 済みなら**孤児化しうる**
// (約定した直後に守りを置けず巻き戻した等)。台帳基準の HoldsPosition だけを見ると、
// 台帳に無い建玉は構造的に発見されない。
func (b *SymbolBundle) IsArmed() bool {
	return b.Configs != nil && b.Configs.IsArmed()
}

func (b *SymbolBundle) HoldsPosition(ctx context.Context) bool {
	if b.PosRepo == nil {
		return true
	}
	open, err := b.PosRepo.ListOpenOrClosing(ctx, b.Symbol)
	if err != nil {
		return true
	}
	return len(open) > 0
}

// 判断が付かないときは false(= poll する)。建玉を持っているかもしれない銘柄を
// 決してスキップしない。
func (b *SymbolBundle) notArmedAndFlat(ctx context.Context) bool {
	if b.Configs != nil && b.Configs.IsArmed() {
		return false // armed
	}
	if b.PosRepo == nil {
		return false
	}
	open, err := b.PosRepo.ListOpenOrClosing(ctx, b.Symbol)
	if err != nil {
		return false
	}
	return len(open) == 0
}

// 約定可能な気配。indicative は同時に下ろす — 売買が再開したのに「気配のみ」の
// バッジが残り続けてはいけない。
func (b *SymbolBundle) setSummary(s *market.MarketSummary) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastSummary = s
	b.lastIndicative = nil
}

func (b *SymbolBundle) setIndicative(s *market.MarketSummary) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastIndicative = s
}

// 約定可能な気配のみ。呼び出し側はこれで建玉を評価するので indicative は返さない。
func (b *SymbolBundle) LastSummary() *market.MarketSummary {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lastSummary
}

// 観測専用。中身は前日終値であって、何かを約定できる価格ではない。
func (b *SymbolBundle) LastIndicative() *market.MarketSummary {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lastIndicative
}
