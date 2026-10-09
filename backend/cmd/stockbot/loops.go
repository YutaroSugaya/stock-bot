package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"stockbot/backend/internal/adapter/apiusage"
	"stockbot/backend/internal/app"
	"stockbot/backend/internal/app/protectiveboard"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/session"
)

// loopConfig tunes the loop intervals.
type loopConfig struct {
	priceInterval     time.Duration
	reconcileInterval time.Duration
	// 口座照会(建玉 reconcile / 維持率)は**建玉を持っている間だけ**回す。無保有の
	// 間は broker に訊く理由が無い(建玉ゼロなら維持率は定数 1.0、建玉照会は空)。
	// 0 = 場中は回さない(寄り前の同期1周だけ)。
	reconcileHeldInterval time.Duration
	marginHeldInterval    time.Duration
	minuteInterval        time.Duration
	live                  bool // 立花-only concerns (token refresh); paper には張り直すセッションが無い
	// reconcile は paper でも回す: 決済 reject で CLOSING のまま座礁した行を
	// resolveStuckClosing が回収する唯一の経路(他のどの経路も CLOSING を再訪しない)。
	reconcile bool
}

// runLoops starts the per-bundle goroutines and blocks until ctx is cancelled.
//
// One synchronous reconcile pass runs per symbol BEFORE any price loop starts:
// after a crash-restart the repo may be empty while the broker holds positions,
// and without this pass the ナンピン禁止 gate sees zero open positions for up to
// reconcileInterval and can double up on a held symbol. A symbol whose pass
// fails holds its new entries until a reconcile succeeds (StartupReconcile).
func runLoops(ctx context.Context, bundles []*app.SymbolBundle, lc loopConfig, logger *slog.Logger) {
	if lc.reconcile {
		for _, b := range bundles {
			safeRun(logger, b.Symbol, "reconcile-startup", func() { b.StartupReconcile(ctx) })
		}
	}
	var wg sync.WaitGroup
	for _, b := range bundles {
		b := b
		wg.Add(1)
		go func() {
			defer wg.Done()
			runTicker(ctx, lc.priceInterval, logger, b.Symbol, "price", func() { b.PriceTick(ctx) })
		}()
		if lc.reconcile {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// 次の待ちと「今回叩いてよいか」は同じ述語から出す(2 本に割ると
				// 必ず片方がずれて、寝ている間に叩くか叩かずに起き続けるかになる)。
				due := func() (time.Duration, bool) {
					// 🛑 「台帳に建玉がある銘柄」だけを照会すると、**台帳に無い建玉は
					// 構造的に発見できない**(reconcile の本業は台帳が知らない建玉を
					// 見つけることなのに、台帳が知っていることを前提にしていた)。
					// arm 済みの銘柄も見る — 発注を試みた銘柄は孤児化しうる唯一の集合で、
					// live の arm は数銘柄なので API 予算にほぼ乗らない。
					w, poll := reconcileSchedule(lc, lc.live, b.Hours.InTradingHours(b.Clock()), b.HoldsPosition(ctx) || b.IsArmed())
					return retryWhileHeld(w, b.EntriesHeldForReconcile()), poll
				}
				iv := func() time.Duration { w, _ := due(); return w }
				runVarTicker(ctx, iv, logger, b.Symbol, "reconcile", func() {
					if _, poll := due(); !poll {
						return // 場外 / 無保有 — broker に訊く理由が無い
					}
					b.ReconcileTick(ctx)
				})
			}()
		}
	}
	wg.Wait()
}

// runDailyLoginLoop は 立花 のセッションを**集計窓(5:30 起点)に 1 回だけ**張り直す。
//
// 🚨 立花はログインを 1 日 1 回に留めるよう求めている(仮想URL は 1 日に 1 度取得すれば
// 該当営業日は継続利用できる)。旧ループは毎時無条件に logout+login していた。
//
// 張り直すのは閉局(03:30)をまたいで動き続けたときだけ: 起動時の login と p_errno=2 の
// 張り直し(adapter 側・間隔つき)で窓の中の login は済んでいる。閉局明けに古い仮想URL が
// p_errno=2 を返す保証は資料に無いので、窓が替わったら自分から 1 回張る。
func runDailyLoginLoop(ctx context.Context, b interface {
	RefreshToken(context.Context) error
}, now func() time.Time, lastLogin func() time.Time, inMaintenance func(time.Time) bool, trip func(reason string), logger *slog.Logger) {
	t := time.NewTicker(dailyLoginCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			dailyLoginTick(ctx, b, now(), lastLogin, inMaintenance, trip, 5*time.Second, logger)
		}
	}
}

// dailyLoginCheckInterval は「窓が替わったか」を見る間隔(見るだけで login は撃たない)。
const dailyLoginCheckInterval = 10 * time.Minute

// needsDailyLogin は now の集計窓にまだ成功した login が無いか。閉局中は張らない。
func needsDailyLogin(now, lastLogin time.Time, inMaintenance func(time.Time) bool) bool {
	if inMaintenance != nil && inMaintenance(now) {
		return false
	}
	return lastLogin.Before(apiusage.WindowStart(now))
}

// dailyLoginTick は 1 回ぶんの判定と張り直し。再試行しても通らなければ緊急停止する
// (閉局明けにセッションが無いまま場に入らない)。shutdown 中の失敗では trip しない。
func dailyLoginTick(ctx context.Context, b interface {
	RefreshToken(context.Context) error
}, now time.Time, lastLogin func() time.Time, inMaintenance func(time.Time) bool, trip func(reason string), baseDelay time.Duration, logger *slog.Logger) {
	if !needsDailyLogin(now, lastLogin(), inMaintenance) {
		return
	}
	logger.Info("daily login: 集計窓が替わったのでセッションを張り直す", "last_login", lastLogin())
	if refreshWithRetry(ctx, b, 4, baseDelay, logger) || ctx.Err() != nil {
		return
	}
	logger.Error("daily login failing after retries; tripping emergency")
	if trip != nil {
		trip("token_refresh_failed")
	}
}

// refreshWithRetry returns true on success or on ctx cancellation (= "don't trip").
func refreshWithRetry(ctx context.Context, b interface {
	RefreshToken(context.Context) error
}, attempts int, baseDelay time.Duration, logger *slog.Logger) bool {
	delay := baseDelay
	for i := 0; i < attempts; i++ {
		if err := b.RefreshToken(ctx); err == nil {
			return true
		} else {
			logger.Warn("token refresh attempt failed", "attempt", i+1, "err", err)
		}
		if ctx.Err() != nil {
			return true // shutting down: do not escalate
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return true
			case <-time.After(delay):
				delay *= 2
			}
		}
	}
	return false
}

func jstHour(now time.Time) int { return now.In(clock.JST).Hour() }

// inTachibanaMaintenanceWindow covers 立花's API-server 閉局 (03:30〜) until the
// morning re-open, so a refresh failure in that window must not trip emergency.
func inTachibanaMaintenanceWindow(now time.Time) bool {
	h := jstHour(now)
	return h >= 3 && h < 6
}

func maintenanceWindowFor(config.BrokerKind) func(time.Time) bool {
	return inTachibanaMaintenanceWindow
}

// runVarTicker is runTicker with a per-iteration interval: iv() is re-evaluated
// after every run(reconcile は建玉の有無で次の待ちが変わる)。
func runVarTicker(ctx context.Context, iv func() time.Duration, logger *slog.Logger, symbol, name string, fn func()) {
	t := time.NewTimer(iv())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			safeRun(logger, symbol, name, fn)
			t.Reset(iv())
		}
	}
}

// runTickerLeading は **起動直後に 1 回撃ってから**周期に入る。
//
// 🚨 通常の runTicker は最初の 1 回を interval ぶん待つ。時間帯で自分を絞るジョブ
// (守りの置き直しは寄り前 07:00〜09:00 だけ)では、これが**静かな取りこぼし**になる:
// 08:55 に起動すると初回チェックが 09:05 = 窓の外になり、その日は丸ごと走らない。
// 「make start するだけでよい」という運用前提が起動時刻によって破れる。
func runTickerLeading(ctx context.Context, interval time.Duration, logger *slog.Logger, symbol, name string, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	runTicksLeading(ctx, t.C, logger, symbol, name, fn)
}

// runTicker drives fn on an interval until ctx is done, isolating panics so one
// symbol cannot take the process down.
func runTicker(ctx context.Context, interval time.Duration, logger *slog.Logger, symbol, name string, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	runTicks(ctx, t.C, logger, symbol, name, fn)
}

// runTicksLeading fires once immediately, then on every tick.
func runTicksLeading(ctx context.Context, ticks <-chan time.Time, logger *slog.Logger, symbol, name string, fn func()) {
	safeRun(logger, symbol, name, fn)
	runTicks(ctx, ticks, logger, symbol, name, fn)
}

// runTicks is the timer-free core: it runs fn on each tick until ctx is done.
func runTicks(ctx context.Context, ticks <-chan time.Time, logger *slog.Logger, symbol, name string, fn func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			safeRun(logger, symbol, name, fn)
		}
	}
}

// panicCounter surfaces recovered panics on /api/status. Set by run() at wiring
// time; nil in tests.
var panicCounter *app.Counters

func safeRun(logger *slog.Logger, symbol, name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			if panicCounter != nil {
				panicCounter.LoopPanics.Add(1)
			}
			logger.Error("loop panic recovered", "symbol", symbol, "loop", name, "panic", r)
		}
	}()
	fn()
}

// realFeedPriceInterval is the price-poll spacing for a real market API polled
// ONE SYMBOL AT A TIME.
//
// 立花には 1 日の API 利用回数の上限がある。1秒 × 監視銘柄数 の価格ループは
// それを容易に超えるので、1銘柄1リクエストのままなら 30 秒まで間引くしかない。
const realFeedPriceInterval = 30 * time.Second

// batchedPriceInterval is the price-poll spacing when quotes are batched into one
// request per tick. **全銘柄がこの1本のレーンで更新される。**
//
// 立花の公式アナウンス(https://www.e-shiten.jp/api/20260310.html)は AM8:00〜
// PM15:30 の「頻繁なポーリング」を控えるよう求めている(回数の上限は非公表)。
// 3秒 = 6,600回/日。詰めるときは必ず dailyQuoteRequestBudget を確認すること
// (逃げ道は STOCKBOT_PRICE_INTERVAL_SEC)。
const batchedPriceInterval = 3 * time.Second

// tradingSessionSeconds is one JPX cash-session day in seconds. 昼休みと場外は
// 価格ループが no-op なので、時価取得の見積りはこの秒数だけで足りる。
const tradingSessionSeconds = 19800

// dailyQuoteRequestBudget caps the quote requests one trading day may cost.
//
// 立花は 1 日あたりの API 利用回数の上限(1 万回以下)を求めている。1日の総数から
// 朝のプール取得(1,551銘柄)・日足リフレッシュ(約250)・ログインを差し引いた
// 残りが場中の枠。
// 上限の内訳は時価 cold 5秒 + hot 2秒の 2 レーン構成で説明がつく(積み上げが
// cold しか数えていないと過小になる — TACHIBANA_API_NOTES.md §1.5-7)。
// 余裕を見て 7000 に置く。引き上げは人間の判断。
const dailyQuoteRequestBudget = 7000

// inSessionQuoteRequests returns an UPPER BOUND on the quote requests the price
// loops cost in one trading day. chunks は監視銘柄が 1 リクエストの上限(120)を
// 何倍超えているか(= broker.QuoteChunks)。
//
// **回数は「レーンの本数 × 頻度 × チャンク数」で決まる**。一括取得は 1リクエストに
// 120銘柄まで積めるので 27銘柄でも 120銘柄でも 1リクエスト/tick だが、**121銘柄目から
// 2リクエストに割れる**。BatchQuoteFeed は実効間隔をチャンク数だけ伸ばすので、
// chunks は約分されて総数は一定になる — その不変条件をここで固定する
// (旧版はチャンク数を数えておらず、実際の通信量が倍でも緑のまま通っていた)。
func inSessionQuoteRequests(lc loopConfig, chunks int) int {
	if chunks < 1 {
		chunks = 1
	}
	perLane := func(iv time.Duration) int {
		if iv <= 0 {
			return 0 // レーン無効
		}
		// 秒に丸めてから割らない: 500ms のような秒未満を int(Seconds()) で
		// 潰すと 0 除算になる(この関数は予算ガードなので落ちてはいけない)。
		return chunks * int(tradingSessionSeconds*time.Second/(iv*time.Duration(chunks)))
	}
	return perLane(lc.priceInterval)
}

// idleReconcileWait is how long a live reconcile goroutine sleeps while there is
// nothing to ask about (場外 or 無保有). 目を覚ますだけで broker は叩かないので
// 予算には乗らない — 寄り付き / 建玉の発生を 1 周期以内に拾うためのもの。
const idleReconcileWait = 5 * time.Minute

// retryWhileHeld は、起動時 reconcile が落ちて新規 entry を保留している銘柄の待ちを
// idleReconcileWait に縮める。保有中の間隔(1 時間)のまま待つと、
// 場中に再起動した日はその銘柄が最大 1 時間建たない。照会は保留中だけ 5 分ごとに増える。
func retryWhileHeld(wait time.Duration, held bool) time.Duration {
	if held && wait > idleReconcileWait {
		return idleReconcileWait
	}
	return wait
}

// reconcileSchedule decides the next reconcile wait and whether this wake-up may
// actually poll the broker.
//
// live が「場中 × 建玉あり」に限られるのは、口座照会が**銘柄ではなく口座**の質問
// だから: 建玉ゼロなら答えは常に空で、場外なら誰も約定しない。24 時間 30 秒ごとに
// 引いていた現行配線は live 1 銘柄で 5,760回/日 を出す(立花の上限は 1 日 10,000回以下)。
//
// paper を据え置くのは紙の帳簿がプロセス内の変数だから: API コストがゼロなので、
// CLOSING 座礁を回収する唯一の経路をわざわざ遅くする理由が無い。
func reconcileSchedule(lc loopConfig, live, inSession, held bool) (wait time.Duration, poll bool) {
	if !live {
		return lc.reconcileInterval, true
	}
	if inSession && held {
		return lc.reconcileHeldInterval, true
	}
	return idleReconcileWait, false
}

// liveClosesPerDayAllowance is the number of settlements a live day is budgeted
// for. 決済のたびに建玉と維持率を取り直すので、予算に織り込む本数が要る。
// live は口座上限が小さい(1〜数ポジ)ので余裕を見て 10 本。
// 実際にこれを超える日は、超えたぶんだけ照会が増える(打ち切らない — 決済直後の
// 再同期は CLOSING 座礁の唯一の回収経路で、予算のために削ってよい守りではない)。
const liveClosesPerDayAllowance = 10

// liveEntryAttemptsPerDayAllowance is the number of entry attempts that reach the
// COLLATERAL gate in one live day. 構造ゲート(建玉枠・ナンピン禁止・セッション)で
// 捨てられる試行は口座照会を払わないので、ここに数えるのは「構造的には建てられる」
// と判断された試行だけ。live は口座上限が小さいので余裕を見て 20 回。
//
// 🛑 **この項が無いと予算が外れる**: 照会が判定より先に無条件で飛ぶ形だと
// 試行数がそのまま回数になり、枠が満杯の live では毎ティック捨てる試行の分だけ
// 余力照会が積み上がる(見積りの数十倍になりうる)。
// 超えた日は超えたぶんだけ増える(打ち切らない — 担保チェックは削ってよい守りではない)。
const liveEntryAttemptsPerDayAllowance = 20

// positionsReqPerPoll / marginReqPerPoll は照会 1 回あたりの wire リクエスト数。
// **2 つのレーンで本数が違う**ので分けてある(同じ数で括っていた頃、余力レーンを
// 1/3 過小に数えていた):
//   - 建玉照会 CLMGenbutuKabuList (+ CLMShinyouTategyokuList)         → 信用有効で 2 本
//   - 余力照会 CLMZanKaiKanougaku (+ CLMZanShinkiKanoIjiritu
//   - CLMZanRealHosyoukinRitu)                                      → 信用有効で 3 本
//
// 建余力(ShinkiKanoIjiritu)は現物余力では信用の可否を判定できないため必須で、
// 減らせない。
func positionsReqPerPoll(marginEnabled bool) int {
	if marginEnabled {
		return 2
	}
	return 1
}

func marginReqPerPoll(marginEnabled bool) int {
	if marginEnabled {
		return 3
	}
	return 1
}

// inSessionAccountRequests returns an UPPER BOUND on the account-lane requests
// (建玉 reconcile + 維持率) one trading day costs.
//
// **時価と違って銘柄数では増えない**: `GetPositions` / `GetAccountMargin` は口座
// 全体の照会なので、LiveQuoteShared の併合キャッシュが N 銘柄を 1 リクエスト/窓に
// 畳む。増えるのは「レーンの本数 × 頻度」だけ — 時価と同じ数え方になる。
//
// 建玉を持たない間はどちらのレーンも回らない前提の数字で、残るのは寄り前の同期
// reconcile 1 周だけ(再起動直後にナンピン禁止ゲートが盲目になる穴を塞ぐ既存の契約。
// CLAUDE.md「live は loop 開始前に同期 reconcile 1周」)。
func inSessionAccountRequests(lc loopConfig, marginEnabled bool, closesPerDay, entryAttemptsPerDay int) int {
	posReq, marginReq := positionsReqPerPoll(marginEnabled), marginReqPerPoll(marginEnabled)
	perLane := func(iv time.Duration, req int) int {
		if iv <= 0 {
			return 0 // レーン無効
		}
		return req * int(tradingSessionSeconds*time.Second/iv)
	}
	total := posReq // 寄り前の同期 reconcile 1 周
	total += perLane(lc.reconcileHeldInterval, posReq)
	total += perLane(lc.marginHeldInterval, marginReq)
	if closesPerDay > 0 {
		// 決済 1 本につき 建玉照会 + 維持率 を取り直す。
		total += closesPerDay * (posReq + marginReq)
	}
	if entryAttemptsPerDay > 0 {
		// 担保ゲートまで来たエントリー試行 1 回につき 余力照会 1 回。
		total += entryAttemptsPerDay * marginReq
	}
	return total
}

// inSessionRequests は場中に立花へ飛ぶ**全レーンの合計**。予算ガードはこれで判定する
// — 時価だけを数えていた頃は、口座照会が実数を5倍にしても緑のまま通っていた。
func inSessionRequests(lc loopConfig, chunks int, marginEnabled bool, closesPerDay, entryAttemptsPerDay int) int {
	return inSessionQuoteRequests(lc, chunks) + inSessionAccountRequests(lc, marginEnabled, closesPerDay, entryAttemptsPerDay)
}

// batchQuoteMaxAge must match the price-loop interval: shorter and every tick
// refetches before the cache is used, longer and bundles trade on a quote older
// than their own poll period.
func batchQuoteMaxAge() time.Duration {
	return defaultLoopConfig(config.ModePaper, config.BrokerPaperLiveFeed, true).priceInterval
}

// defaultLoopConfig derives loop intervals from the bot mode and broker.
// batched = 時価取得が1ティック1リクエストに畳まれているか。畳まれていない実フィード
// は 30 秒に間引く — ここを取り違えると高負荷の指摘を再発させる。
func defaultLoopConfig(mode config.Mode, broker config.BrokerKind, batched bool) loopConfig {
	price := time.Second
	if broker.ServesKlines() {
		price = realFeedPriceInterval
		if batched {
			price = batchedPriceInterval
		}
	}
	if broker.ServesKlines() {
		if v := os.Getenv("STOCKBOT_PRICE_INTERVAL_SEC"); v != "" {
			// 壊れた値は既定に落とす(env の typo で 0 秒ループ = 全力ポーリング
			// に戻るのが最悪)。
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				price = time.Duration(n) * time.Second
			}
		}
	}
	return loopConfig{
		priceInterval:     price,
		reconcileInterval: 30 * time.Second,
		// 保有中だけ 1 時間に 1 回。値段の変動はローカル
		// 計算で追えるので、同じ質問を 30 秒ごとに broker へ投げる理由が無い。
		// 30 秒のままだと live 1 銘柄で建玉照会だけで 5,760回/日 になり、時価と
		// 合わせて立花の上限(1 日 10,000回以下)を単独で超える。
		reconcileHeldInterval: time.Hour,
		marginHeldInterval:    time.Hour,
		minuteInterval:        time.Minute,
		live:                  mode == config.ModeLive,
		reconcile:             true,
	}
}

// runTracks runs the research loops plus (when present) the live track's
// loops concurrently, blocking until all of them stop.
//
// `runLoops` は `wg.Wait()` でブロックするので、追加トラックは goroutine 化して全部の
// 終了を待つ形にする。ctx は共有 — SIGTERM で全部止まる。
//
// 🛑 追加トラックが 1 本も無いときは **research の runLoops を直接呼ぶ**(goroutine も
// WaitGroup も挟まない)。既定経路にスケジューリングの差を持ち込まないため。
//
// トラックは research と live の 2 本だけ。
func runTracks(ctx context.Context, bundles []*app.SymbolBundle, lc loopConfig,
	live *liveTrack, logger *slog.Logger) {
	if live == nil {
		runLoops(ctx, bundles, lc, logger)
		return
	}
	var wg sync.WaitGroup
	if live != nil {
		// live の銘柄選定(決定論・LLM 非関与)。空き枠のぶんだけ arm し直す。
		if live.selector != nil {
			go runTicker(ctx, live.rearmInterval, logger, "live-universe", "selector",
				func() { live.selector.Tick(ctx) })
		}
		// 🛑 守りの注文期日を切れる前に延ばす。live だけ。
		// 立花の sOrderExpireDay は最大 10 営業日なので、放っておくと多日保有の建玉は
		// 9 営業日目に SL を失って裸で残る。
		//
		// 🚨 **訂正ではなく置き直し**。「10営業日迄」は**発注日起点**なので、訂正では
		// 元の注文の天井を超えられない —— 天井より先の期日を投げても拒否されるだけ。
		// 訂正ループは**構造的に成功しえない**ので持たず、寄り前の置き直しで延ばす。
		//
		// 🛑 **Leading**(起動直後に 1 回)。窓が 2 時間しかないので、通常の ticker だと
		// 窓の終わり際に起動した日を丸ごと取りこぼす。窓の外なら中で no-op に落ちる。
		if live.replaceProtective != nil {
			go runTickerLeading(ctx, replaceProtectiveInterval, logger, "live", "protective-expiry",
				func() { runReplaceProtectiveExpiry(ctx, live, logger) })
		}
		// 🚨 板の守りの写しを更新する(画面の TP/SL を実体で出すため)。
		// 🛑 **人間が見る時間帯だけ**回す。立花のレート予算は日次で有限なので、
		// 深夜に写しを取り直す価値は無い(建玉の数だけ照会が飛ぶ)。
		go runTickerLeading(ctx, protectiveBoardInterval, logger, "live", "protective-board",
			func() { refreshProtectiveBoardIfWatched(ctx, live, logger) })
		// 🚨 **板から消えた守りを自動で置き直す**。
		// 多日の守りは翌日以降 broker 側で失効しうる(値幅制限の帯がずれて脚が外に出ると
		// 「繰越失効」で注文ごと無効になる)。置き直し(ReplaceProtectiveOrder)は
		// **板に注文が在ること**が前提なので消えた守りを拾えず、arm は人間の手動経路だった。
		// その結果、live の建玉が丸 1 日ぶん裸で放置されうる。
		// 🛑 **時間帯で縛らない。** 置くだけの操作で守りが消える窓を開けないので、
		// 裸に気づいたのが 10:00 なら 10:00 に置くのが正しい(arm と同じ規律)。
		if live.rearmProtective != nil {
			go runTickerLeading(ctx, rearmProtectiveInterval, logger, "live", "protective-rearm",
				func() { runRearmProtective(ctx, live, logger) })
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			runLoops(ctx, live.bundles, live.loopCfg, logger)
		}()
	}
	runLoops(ctx, bundles, lc, logger)
	wg.Wait()
}

// 守りの置き直しループの時間設計(朝の時間帯・残り営業日が閾値を切ったら)。
//
// 🛑 **寄り前だけ**。取消と再発注の間は守りが完全に消えるので、値が動く時間帯に
// その窓を開けない。**引け後ではなく寄り前**なのは、失敗したときに人間が寄りまでに
// 手当てできる時間が残るから —— 引け後に失敗すると裸で一晩持ち越すことになる。
// 深夜を外すのは broker のメンテ窓に当たると照会も発注も落ちるため。
//
// 周期 10 分は「窓(2時間)の中で必ず数回は回る」ための値。閾値が営業日単位なので
// 分単位の精度は要らないが、起動が遅れた朝でも 1 度は回る余裕が要る。
const (
	replaceProtectivePreOpenLead = 2 * time.Hour // 寄り 09:00 の 2 時間前 = 07:00〜
	replaceProtectiveInterval    = 10 * time.Minute
)

// protectiveBoardInterval は板の写しの更新周期。**人間が画面を見ている間だけ**
// 回す(寄り前 + 場中)。建玉の数だけ照会が飛ぶので、深夜に取り直す価値は無い。
const protectiveBoardInterval = 15 * time.Minute

// rearmProtectiveInterval は「板から消えた守り」を拾い直す周期。
// 🛑 板の写し(15 分)より**短くしない** —— 置き直しは照会 + 発注なので、
// 失敗が続く銘柄があると立花のレート予算をそのぶん食う。裸は毎周の ERROR で見える。
const rearmProtectiveInterval = 15 * time.Minute

// runRearmProtective は 1 周ぶん。**守りを置くだけ**なので emergency 中でも走る。
func runRearmProtective(ctx context.Context, live *liveTrack, logger *slog.Logger) {
	// 🛑 寄り前の手当て(置き直し → 引き上げ)の取消と再発注の間に割り込まない。
	live.protectiveMu.Lock()
	defer live.protectiveMu.Unlock()
	if live.rearmProtective == nil {
		return
	}
	res, errs := live.rearmProtective.Execute(ctx)
	for _, e := range errs {
		logger.Error("🚨 守りの自動復旧に失敗 — **この建玉はまだ裸**", "track", "live", "err", e)
	}
	if res.Armed > 0 {
		logger.Warn("板から消えていた守りを自動で置き直した", "track", "live",
			"armed", res.Armed, "skipped", res.Skipped,
			"🛑", "置き直しが要ったこと自体が異常。broker 側で失効した理由を追うこと")
	}
}

// 板の写しを更新する時間帯。**人間が画面を見ている間**を素直に時刻で表す。
//
// 🚨 ここを `InTradingHours || 寄り前` の和集合で書くと、**昼休み
// (11:30〜12:30)がどちらにも入らない**(昼休みに再起動すると板を一度も読めず、
// 画面が台帳の凍結値のまま 12:30 まで固まる)。
// 「取引時間の和集合」で日中を表そうとすると**場の切れ目が必ず落ちる**。
//
// 深夜を外すのは、建玉の数だけ照会が飛ぶうえ broker のメンテ窓に当たるから。
const (
	protectiveBoardFromHour = 7  // 07:00〜 寄り前から
	protectiveBoardToHour   = 16 // 〜15:59 引け(15:30)の少し後まで
)

func shouldRefreshProtectiveBoard(hours session.TradingHours, now time.Time) bool {
	if !hours.IsTradingDay(now) {
		return false
	}
	h := now.In(clock.JST).Hour()
	return h >= protectiveBoardFromHour && h < protectiveBoardToHour
}

// refreshProtectiveBoardIfWatched は日中だけ板を読み直す。
// 🛑 窓の外では**直前の写しをそのまま残す**(消すと「守りが無い」と区別できない)。
// 画面は fetched_at を必ず出すので、古い写しを現在の実体と読むことはない。
func refreshProtectiveBoardIfWatched(ctx context.Context, live *liveTrack, logger *slog.Logger) {
	now := live.clock()
	if !shouldRefreshProtectiveBoard(live.hours, now) {
		return
	}
	protectiveboard.Refresh(ctx, live.store.positions, live.broker, &live.boardState, now, logger)
}

// shouldReplaceProtectiveNow は「今この瞬間に置き直しを撃ってよいか」。
//
// 🛑 **ここと usecase の両方で場中を弾く。**片方だけだと、配線を変えたときに
// 黙って場中に撃つ形になりうる(守りが消える窓を値動きの中で開ける)。
func shouldReplaceProtectiveNow(hours session.TradingHours, now time.Time) bool {
	return hours.InPreOpenWindow(now, replaceProtectivePreOpenLead)
}

// runReplaceProtectiveExpiry は 1 周ぶん。**窓の外では broker に訊かない**
// (照会も立花のレート予算を食う)が、撃った結果は必ずログに出す。
//
// 🛑 失敗を握り潰さない。「延ばしたつもりで切れていた」と「取消は通ったのに置けて
// いない」が最悪の形なので、エラーは 1 本ずつ Error で出す —— 人間が朝ログで
// 気づける唯一の面。再発注の失敗は usecase 側が emergency を trip する。
func runReplaceProtectiveExpiry(ctx context.Context, live *liveTrack, logger *slog.Logger) {
	now := live.clock()
	if !shouldReplaceProtectiveNow(live.hours, now) {
		return
	}
	// 🛑 置き直し → 引き上げを 1 本の流れで回し、その間は守りの自動復旧を待たせる。
	live.protectiveMu.Lock()
	defer live.protectiveMu.Unlock()
	res, errs := live.replaceProtective.Execute(ctx)
	live.replaceState.set(now, res, errs)
	for _, err := range errs {
		logger.Error("守りの置き直しに失敗", "track", "live", "err", err)
	}
	if res.Replaced > 0 {
		logger.Info("守りの注文期日を延ばした(取消 → 再発注)", "track", "live", "orders", res.Replaced)
	}
	// 🚨 trail の利確の線を板の SL にも置く。期日の置き直しの**後**に回す。
	if live.raiseTrailStops == nil {
		return
	}
	raised, rerrs := live.raiseTrailStops.Execute(ctx)
	for _, err := range rerrs {
		logger.Error("板の SL の引き上げに失敗", "track", "live", "err", err)
	}
	if raised.Raised > 0 {
		logger.Info("板の SL を trail の利確の線まで引き上げた(取消 → 再発注)", "track", "live", "orders", raised.Raised)
	}
}
