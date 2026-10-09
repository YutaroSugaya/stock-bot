package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"stockbot/backend/internal/advisor"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/command"
)

// LLM は**発注経路には絶対に入らない** — 出力は決定論エンジンが走らせる config だけ。
// 有効時は決定論 Selector の auto-arm を**置き換える**(cmd 側で Selector の ticker を
// 止める)。両方動かすと holder を取り合う。
type AdvisorLoop struct {
	Symbols    []string
	Candles    port.CandleRepository
	Screeners  []strategy.Screener
	Advisor    port.Advisor
	HardLimits *config.HardLimits
	Menu       []config.StrategyName
	Hours      session.TradingHours
	Clock      func() time.Time

	// Arm の error は arm をスキップする(fail-close)。
	Arm func(*config.StrategyConfig) error

	// BuildConfig != nil のとき **LLM を一切呼ばず**、決定論のテンプレートで arm する。
	// 差し替えるのは `AdvisorCycle.Run` の 1 点だけで、
	// universe → RankCandidates → 枠配布 → Arm は元から決定論。
	//
	// 🚨 なぜ外すか: 実行に効く**変動する** LLM 出力はほぼ無く、生きた出力は
	// 「構える / 構えない」の 1 ビットだけで、それもほとんどが「構える」。
	// 複数のアームを公平に測るとは戦略以外の要因を取り除くことなので、LLM の裁量
	// (= 交絡)は公平性を追求するほど必然的に剥がれる。研究モードは
	// 「トリガー全採用」で動く。
	BuildConfig func(symbol string, slot config.StrategyName, daily []market.Candle) (*config.StrategyConfig, error)

	// 建玉中の **(銘柄, 戦略)** は再 arm も disarm もしない(建玉は entry 時に凍結した
	// config で走っている)。nil = 何も建玉中でないとみなす。
	// 🛑 キーに戦略を含めるのが要 — 銘柄だけで見ると、片方のアームが建った瞬間に
	// 同じ銘柄のもう片方は**永久に arm されない**(兄弟アームのペアが 0 本になる原因)。
	IsHeld func(ctx context.Context, symbol string, name config.StrategyName) bool

	// ShortAllowed は「この銘柄の売建を出せるか」(貸借銘柄ホワイトリスト)。
	// nil = 判定しない(従来どおり)。
	//
	// 🚨 枠を配る**前**に売り候補を間引くために要る。発注前ゲートは arm の後なので、
	// 出せない売りが per_strategy_n の枠を占有し、同じ戦略の買い候補が arm されない
	// (「買いには一切効かない」という約束が実装で破れる)。
	ShortAllowed func(symbol string) bool

	// Disarm が無いと armed 集合が場中ずっと単調増加する(09:00 の pick が、
	// advisor がとうに離れた 14:00 でも live のまま)。nil = disarm しない。
	// キーは IsHeld と同じく (銘柄, 戦略)。
	Disarm func(symbol string, name config.StrategyName) error

	// 資金キャパシティ・フィルタ。最小単元の建玉金額が
	// これを超える銘柄は universe から落とす。0/負 = 無効。
	// paper 残高は大きくても実弾の口座はそれよりはるかに小さいので、paper の勝ちが
	// 実弾では張れない金額の建玉に集中すると、実弾で再現できない成績になる。口座残高を
	// 絞ると標本が数件に落ちるので、**残高はそのまま銘柄の価格帯だけを絞る**。
	MaxPositionNotionalJPY int

	TopN int
	// 戦略あたりの上限。0/負 = 1。全体の上限は TopN。
	PerStrategyN int
	// 同時に走らせる LLM(claude サブプロセス)の本数。0/負 = 1(直列)。
	// 1銘柄あたり実測 621 秒(Opus 5/max)なので、直列だと top_n=10 のラウンドが
	// 場中に終わらない。銘柄ごとの生成は独立(共有状態なし)なので並列にできる。
	MaxConcurrent int
	Notifier      port.Notifier
	Runs          port.AdvisorRunRepository
	// 全銘柄 × 全スクリーナーの毎ラウンド記録(best-effort)。advisor_runs には
	// 選ばれた銘柄しか残らないので、「枠に入らなかった候補のその後」の検証は
	// この記録だけが可能にする。
	Screens port.ScreenSnapshotRepository
	Logger  *slog.Logger

	// 静穏期間の上限。新しいトリガーが無くても場中はこの間隔で必ず走る。0 = 1h。
	Heartbeat time.Duration

	// SnapshotInterval は `screen_snapshots` の**書込周期**。0 = arm ラウンドごと(従来)。
	//
	// 🚨 heartbeat を 60分 → 1分 にすると arm ラウンドは 1 日 6 回 → 約 270 回になる。
	// snapshot は 1 ラウンド **2,400 行**(200銘柄 × **12** スクリーナー)なので、周期を
	// 共有したままだと **648,000 行/日**。ランキングは日足由来で日中不変なので、
	// **周期を分離すれば 1日 約14,400 行**に収まる。
	// arm を速くするための変更が、測定の副産物で DB を膨らませることを防ぐ 1 行。
	SnapshotInterval time.Duration
	lastSnapshotAt   time.Time
	// 最後に書いた picked 集合。**唯一日中に変わる列**なので、周期の内側でも
	// 変化したら書く(shouldSnapshot)。
	lastPicked map[armSlot]bool

	// >0 で寄り前ウォームアップ窓: 寄りの lead 分前にその日の最初の LLM ラウンドを
	// 前倒しし、寄り付き時点で arm 済みにする。入力は前日確定足なので判断材料は
	// 9:00 と同一 — 寄り直後の初動を LLM レイテンシで取り逃さないため。0 = 場中のみ。
	PreOpenLead time.Duration

	// arm ゼロが続いた連続回数。壊れた/利用上限に達した advisor を、毎ティックの
	// Warn に埋もれさせず Error へ昇格させる。
	failStreak int

	// ticker(Tick)と dashboard の手動トリガー(StartManual)が LLM を同時に
	// 走らせないための直列化(トークンの二重払い・状態競合の防止)。
	mu sync.Mutex

	lastAdviseAt time.Time
	// lastAdviseSig / quietRound は「前回と同じ状態か」の判定用(ログ量の抑制だけに使う)。
	lastAdviseSig string
	quietRound    bool
	// outOfSession は「場外スキップを既に 1 回出したか」。ログ量の抑制だけに使う。
	outOfSession bool
	// armedSeen は (銘柄, 戦略) → 既に Info で出した config_id。ログ量の抑制だけに使う。
	armedSeen map[armSlot]string
	// 前スキャン時点で既に成立していた (symbol|strategy)。トリガーの**立ち上がり**
	// でだけ発火する(成立しっぱなしの候補が毎分再発火しない)。cooldown も日次上限も
	// 無く、新しいトリガーは即発火。
	prevTriggered map[string]bool
	advisedDay    string
	// 前ラウンドで arm した **(銘柄, 戦略)**。advisor が離れたアームを残さず disarm するため。
	armedPrev map[armSlot]bool
}

// armSlot は (銘柄, 戦略) キー。arm / disarm / 建玉判定はすべてこの粒度。
type armSlot struct {
	Symbol   string
	Strategy config.StrategyName
}

const failStreakAlertThreshold = 3

// heartbeatSlack は heartbeat 判定の許容差。
//
// 🚨 heartbeat は **1分**と事前に決めてあるが、スキャン周期も
// 60 秒なので、素の `>= hb` だとティック配送のゆらぎで**半分のティックが 60 秒を
// わずかに下回って落ち**、実効周期が約 90 秒になる。
// 決めた数字と実際の建玉時刻分布がずれる。
//
// 周期の 1/10、ただし**上限 1 秒**。ゆらぎはミリ秒〜数十ミリ秒なので 1 秒で十分に足り、
// 上限があるので周期が長いほど相対的な許容差は小さくなる(60 分周期なら 1/3600)。
// 事前登録値の `interval_min: 1` では 1 秒 = 周期の 1/60 で、
// **周期そのものを実質短くしない**(「毎ティック」に化けない)。
func heartbeatSlack(hb time.Duration) time.Duration {
	if slack := hb / 10; slack < time.Second {
		return slack
	}
	return time.Second
}

const defaultHeartbeat = time.Hour

var ErrAdviseBusy = errors.New("advise round already running(LLM 実行中 — 少し待って再実行)")

// Tick は1回の**スキャン**(≈1/分で呼ぶ前提)。ランキングはキャッシュ日足の算術で
// LLM トークンを使わない。LLM が走るのは shouldAdvise が理由を返したときだけ。
func (l *AdvisorLoop) Tick(ctx context.Context) {
	if !l.mu.TryLock() {
		return // 前の advise(LLM 最長 timeout_seconds)が実行中 — 重ねない
	}
	defer l.mu.Unlock()
	now := l.now()
	preOpen := l.Hours.InPreOpenWindow(now, l.PreOpenLead)
	if !preOpen && (!l.Hours.IsTradingDay(now) || !l.Hours.InTradingHours(now) || l.Hours.IsAfterEntryCutoff(now)) {
		// 🛑 **場外の間は 1 回だけ Info。** 1 分周期なので毎回出すと昼休みだけで 60 行、
		// 1 日で数百行になり本当に見たいログが埋もれる。
		// 🛑 無音にはしない — 「いま場外だから止まっている」が読めないと、
		// 故障で止まっているのと区別が付かない。入った最初の 1 回は必ず出す。
		if l.outOfSession {
			l.debug("advisor_loop_skip_out_of_session", "at", now.Format(time.RFC3339))
		} else {
			l.log("advisor_loop_skip_out_of_session", "at", now.Format(time.RFC3339))
			l.outOfSession = true
		}
		return
	}
	// 場中に戻った。次に場外へ出たときはまた 1 回出す。
	l.outOfSession = false
	universe := l.universe(ctx)
	ranked := strategy.RankCandidates(universe, l.screeners())
	if len(ranked) == 0 {
		l.log("advisor_loop_empty_universe")
		return
	}

	reason := l.shouldAdvise(now, ranked)
	if reason == "" {
		return // nothing new since the last scan — no LLM call (cheap path)
	}
	l.advise(ctx, now, universe, ranked, reason)
}

// 手動ラウンドを**背景で**1回起動する(LLM は最長 timeout_seconds 走るので HTTP を
// 待たせない)。ctx には bot の run ctx を渡す — 停止(SIGTERM)で claude サブプロセスも
// 道連れに cancel される。二重起動は ErrAdviseBusy(トークンの二重払い防止)。
func (l *AdvisorLoop) StartManual(ctx context.Context) error {
	if !l.mu.TryLock() {
		return ErrAdviseBusy
	}
	go func() {
		defer l.mu.Unlock()
		// ボタン一発の panic で常時稼働の bot を落とさない。defer は LIFO なので
		// recover が先に走り、Unlock は必ず実行される。
		defer func() {
			if r := recover(); r != nil {
				l.log("advisor_manual_panic", "panic", fmt.Sprint(r))
			}
		}()
		l.adviseManual(ctx)
	}()
	return nil
}

// 手動ラウンドはセッションゲートも rising-edge / heartbeat も通さない(人間の観測用)。
// 場外で arm されても、エントリーは決定論 risk gate が場中しか許さない。
func (l *AdvisorLoop) adviseManual(ctx context.Context) {
	now := l.now()
	universe := l.universe(ctx)
	ranked := strategy.RankCandidates(universe, l.screeners())
	if len(ranked) == 0 {
		l.log("advisor_manual_empty_universe")
		return
	}
	// 場外の手動実行が当日の session_open/pre_open 発火や heartbeat 基準を消費しない
	// よう、判定状態を退避して復元する(手動が失敗しても自動の1回目は発火する)。
	// 場中と寄り前窓では復元しない — 同一データで自動ラウンドを重ねるのは LLM 代の無駄。
	keepState := (l.Hours.IsTradingDay(now) && l.Hours.InTradingHours(now)) ||
		l.Hours.InPreOpenWindow(now, l.PreOpenLead)
	restoreDay, restoreLast := l.advisedDay, l.lastAdviseAt
	l.shouldAdvise(now, ranked) // rising-edge 基準は更新(同一トリガーの二重発火防止)
	l.advise(ctx, now, universe, ranked, "manual")
	if !keepState {
		l.advisedDay, l.lastAdviseAt = restoreDay, restoreLast
	}
}

// caller holds l.mu.
func (l *AdvisorLoop) advise(ctx context.Context, now time.Time, universe map[string][]market.Candle, ranked []strategy.Candidate, reason string) {
	l.lastAdviseAt = now
	// 🛑 **何も起きないラウンドを毎回出さない**。
	// 決定論 arm は heartbeat 1分なので、枠が埋まっていると
	// 同じ 2 行を毎分吐き続ける(午前だけで 100 回を超える)。本当に見たいログ
	// (守りの期日訂正の失敗など)が埋もれる。
	//
	// 🛑 ただし**無音にしない**。「何も起きていない」と「壊れて回っていない」は
	// 別の状態で、後者に気づけなくなるのが最悪。出すのは
	//   - heartbeat 以外の理由(pre_open / session_open / new_trigger)= 必ず出す
	//   - heartbeat でも**首位が入れ替わった**とき
	// で、それ以外は Debug に落とす(-v で追える)。
	// 署名は **状態だけ**(理由を混ぜない)。混ぜると session_open → 最初の heartbeat が
	// 必ず 1 回鳴り、実質毎日ノイズが戻る。理由の変化は下の `quiet` 条件が見ている。
	sig := ranked[0].Symbol + "|" + string(ranked[0].Strategy)
	quiet := strings.HasPrefix(reason, "heartbeat") && sig == l.lastAdviseSig
	l.lastAdviseSig = sig
	if quiet {
		l.debug("advisor_loop_advise", "reason", reason, "top", ranked[0].Symbol,
			"triggered", ranked[0].Triggered, "score", ranked[0].Score)
	} else {
		l.log("advisor_loop_advise", "reason", reason, "top", ranked[0].Symbol,
			"triggered", ranked[0].Triggered, "score", ranked[0].Score)
	}
	l.quietRound = quiet

	n := l.TopN
	if n <= 0 {
		n = 1
	}
	isHeld := func(sym string, name config.StrategyName) bool {
		return l.IsHeld != nil && l.IsHeld(ctx, sym, name)
	}
	// 枠の配り方と巡回の先頭ずらしの理由は selectAdvisePicks を見る。
	// 🛑 **発火ゼロなら arm ゼロ**。ここには以前 `picks = ranked[:1]` という枝があった。
	// LLM 経路では「何も発火しなかった日に最上位銘柄を LLM に見せて `no_trade` と
	// 言わせ、その理由を記録する」ための**日記の枝**で、実害は無かった。
	// 決定論 arm は `no_trade` を答えない —— `ArmTemplate.Build` は `Triggered` を
	// 見ずに必ず生きた config を作るので、この枝は「**発火していない銘柄を毎日 1 つ
	// arm する**」に化ける。「トリガー成立 → 即 arm」
	// と食い違い、「発火した候補だけを測る」という母集団の定義そのものを壊す。
	picks := selectAdvisePicks(ranked, isHeld, n, l.PerStrategyN, now.YearDay(), l.ShortAllowed)
	// 周期が来たら全ランキングを、picked だけ変わったなら**変わったぶんだけ**書く。
	// 🚨 当初は picked 変化でも全ラウンド(200銘柄 × 12スクリーナー = 2,400 行)を
	// 書いていた。`ranked` は日足由来で日中不変なので picked を動かすのは
	// **建玉の出入りだけ**だが、それは 1 日に数十回起きる —— 「変化は稀なので
	// 書込量は増えない」という見立ては誤りで、1 日 10 万行規模になる。
	// picked は最大 top_n 行なので、差分だけなら 1 回あたり数十行で済む。
	// 🛑 **判定は shouldSnapshot 経由**。以前はここに同じ条件を書き下していたので、
	// 事前登録した検出器(`TestSnapshotAlsoWritesWhenThePickedSetChanges`)は
	// production が呼ばない関数を叩いていた = **本番経路は何も縛られていなかった**
	// (実装自体は正しくても、テストが守っているのは別物になる)。
	switch {
	case !l.shouldSnapshot(now, picks):
		// 周期の内側で picked も変わっていない = 書かない。
	case l.dueSnapshot(now):
		l.persistScreenSnapshots(ctx, now, ranked, picks)
		l.markSnapshot(now, picks)
	default: // picked だけが変わった → 差分だけ書く
		l.persistScreenSnapshots(ctx, now, newlyPicked(l.lastPicked, picks), picks)
		l.lastPicked = pickedSet(picks)
	}
	results := l.buildConfigs(ctx, universe, picks, now)

	// arm は ranked 順に直列(「同じ入力なら同じ順に arm される」再現性のため)。
	armedAny := false
	armedNow := make(map[armSlot]bool, len(picks))
	for _, cfg := range results {
		if cfg == nil {
			continue
		}
		// 単元より大きい株数で上限を超える config は arm しない(第二の関門)。
		if err := l.withinNotionalCap(cfg, universe[cfg.Symbol]); err != nil {
			l.log("advisor_loop_arm_rejected_notional", "symbol", cfg.Symbol, "err", err.Error())
			continue
		}
		if err := l.Arm(cfg); err != nil {
			l.log("advisor_loop_arm_rejected", "symbol", cfg.Symbol, "err", err.Error())
			continue
		}
		armedAny = true
		armedNow[armSlot{Symbol: cfg.Symbol, Strategy: cfg.StrategyName}] = true
		// 🛑 **変わったときだけ Info。** arm は毎ラウンド作り直されるので、そのまま
		// 出すと同じ (銘柄, 戦略) が何十回も「armed」と宣言される
		// (同一銘柄が 1 日に何十回も出る)。
		// 🛑 無音にしない — **新規 arm と config_id の変化は必ず出す**(何を構えたかは
		// 監査に要る)。変わっていないラウンドだけ Debug へ落とす。
		slot := armSlot{Symbol: cfg.Symbol, Strategy: cfg.StrategyName}
		if l.armedSeen[slot] == cfg.ConfigID {
			l.debug("advisor_loop_armed", "symbol", cfg.Symbol,
				"strategy", string(cfg.StrategyName), "config_id", cfg.ConfigID)
		} else {
			l.log("advisor_loop_armed", "symbol", cfg.Symbol,
				"strategy", string(cfg.StrategyName), "config_id", cfg.ConfigID)
		}
		if l.armedSeen == nil {
			l.armedSeen = map[armSlot]string{}
		}
		l.armedSeen[slot] = cfg.ConfigID
	}
	// disarm された枠は armedSeen からも落とす — 次に同じ銘柄が arm されたら
	// それは**新しい arm** なので Info で出す(落とさないと永久に Debug のまま)。
	for slot := range l.armedSeen {
		if !armedNow[slot] {
			delete(l.armedSeen, slot)
		}
	}
	l.disarmStale(ctx, armedNow)
	if reason != "manual" {
		// 手動ラウンドは failStreak に混ぜない(人間の試行で異常 Error を鳴らさず、
		// 手動の成功 arm が自動経路の異常 streak を隠すこともない)。
		l.trackStreak(ctx, armedAny, len(picks) > 0)
	}
}

// buildConfigs は picks から arm 候補の config を作る。決定論経路(BuildConfig)は
// ミリ秒なので**直列**、LLM 経路は 1 銘柄 621 秒(Opus 5/max)なので**並列**。
// どちらも戻り値は picks と同じ添字順 — arm は後段で ranked 順に直列で行うので、
// 生成の完了順が実行ごとに揺れても発注順は動かない。
func (l *AdvisorLoop) buildConfigs(ctx context.Context, universe map[string][]market.Candle, picks []strategy.Candidate, now time.Time) []*config.StrategyConfig {
	results := make([]*config.StrategyConfig, len(picks))
	if l.BuildConfig != nil {
		// 静かなラウンド(heartbeat かつ首位不変)では Debug へ。arm が実際に起きた
		// ラウンドは下の advisor_loop_armed / picked 差分が残るので、証跡は消えない。
		if l.quietRound {
			l.debug("advisor_loop_generate_begin", "slots", len(picks), "mode", "deterministic")
		} else {
			l.log("advisor_loop_generate_begin", "slots", len(picks), "mode", "deterministic")
		}
		for i, cand := range picks {
			if l.IsHeld != nil && l.IsHeld(ctx, cand.Symbol, cand.Strategy) {
				l.log("advisor_loop_skip_held", "symbol", cand.Symbol, "strategy", string(cand.Strategy))
				continue
			}
			cfg, err := l.BuildConfig(cand.Symbol, cand.Strategy, universe[cand.Symbol])
			if err != nil {
				l.log("advisor_loop_generate_error", "symbol", cand.Symbol,
					"strategy", string(cand.Strategy), "err", err.Error())
				continue
			}
			results[i] = cfg // nil = 作れなかった — fail-close
		}
		return results
	}
	return l.generateWithLLM(ctx, universe, picks, now, results)
}

// generateWithLLM は LLM 時代の経路。**削除ではなく残す**: 戦略が
// live_allowed_strategies に入った後の日次 go/no-go には正当な用途がある。
func (l *AdvisorLoop) generateWithLLM(ctx context.Context, universe map[string][]market.Candle, picks []strategy.Candidate, now time.Time, results []*config.StrategyConfig) []*config.StrategyConfig {
	conc := l.MaxConcurrent
	if conc <= 0 {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	// 決定論経路と同じ規則(上のコメント参照)。LLM 経路は場中には使われないが、
	// 片方だけ静かにすると「どちらの経路で走っているか」でログ量が変わり、
	// 静かな日と壊れた日の区別がさらに付きにくくなる。
	if l.quietRound {
		l.debug("advisor_loop_generate_begin", "symbols", len(picks), "max_concurrent", conc, "mode", "llm")
	} else {
		l.log("advisor_loop_generate_begin", "symbols", len(picks), "max_concurrent", conc, "mode", "llm")
	}
	for i, cand := range picks {
		i, sym, daily, slot := i, cand.Symbol, universe[cand.Symbol], cand.Strategy
		// 建玉中は再 arm しない(entry 時に凍結した config で走っている)。判定は
		// **(銘柄, 戦略)** — 銘柄で見ると兄弟アームまで巻き添えで止まる。
		if l.IsHeld != nil && l.IsHeld(ctx, sym, slot) {
			l.log("advisor_loop_skip_held", "symbol", sym, "strategy", string(slot))
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// 1銘柄の失敗が他の銘柄を巻き込まない(panic も含めて封じ込める)。
			defer func() {
				if r := recover(); r != nil {
					l.log("advisor_loop_generate_panic", "symbol", sym, "panic", fmt.Sprint(r))
				}
			}()
			// typed-nil 対策: nil の *slog.Logger をそのまま interface に入れると
			// 受け側の nil チェックを素通りして panic する。
			var cycleLogger command.InfoLogger
			if l.Logger != nil {
				cycleLogger = l.Logger
			}
			cycle := &command.AdvisorCycle{
				Advisor: l.Advisor,
				// 枠の戦略拘束の hard gate(プロンプト側の指示は soft)。拘束前は入口が包含関係の
				// 戦略が LLM の選好で横取りされ、特定の戦略の選択が 0 回になりうる。
				Promoter:     &command.Promoter{HardLimits: l.HardLimits, ExpectedSymbol: sym, Menu: l.Menu, ExpectedStrategy: slot},
				BuildSummary: func() *market.MarketSummary { return l.summaryFor(sym, daily, now, slot) },
				Notifier:     l.Notifier,
				Runs:         l.Runs,
				Logger:       cycleLogger,
			}
			cfg, _, err := cycle.Run(ctx)
			if err != nil {
				l.log("advisor_loop_generate_error", "symbol", sym, "err", err.Error())
				return
			}
			results[i] = cfg // nil = non-promotable / rejected — fail-close
		}()
	}
	wg.Wait()
	return results
}

// shouldSnapshot は screen_snapshots を今書くか。0 = 毎ラウンド(従来)。
// その日の 1 回目は必ず書く(lastSnapshotAt は zero)。
//
// 🚨 **picked が変わったら周期の内側でも書く**。周期分離の根拠は
// 「ランキングは日中ほぼ不変で、変わるのは picked 列だけ」だったが、分離した
// ことで**その唯一変わる列**が 1日約270ラウンド中 6 回しかサンプルされなくなっていた。
// 日次総評パケットは `count(DISTINCT symbol) FILTER (WHERE picked)` を「何枠取れたか」
// として読む(pg/journal_repo.go)ので、実際の arm 数を大きく下回る数字が出る。
// picked の変化は稀なので、これを足しても書込量は増えない(当時挙げていた代替案)。
func (l *AdvisorLoop) shouldSnapshot(now time.Time, picks []strategy.Candidate) bool {
	return l.dueSnapshot(now) || !samePickedSet(l.lastPicked, picks)
}

// dueSnapshot は**周期**で書く番か(その日の 1 回目は必ず書く)。
func (l *AdvisorLoop) dueSnapshot(now time.Time) bool {
	if l.SnapshotInterval <= 0 {
		return true
	}
	return l.lastSnapshotAt.IsZero() || now.Sub(l.lastSnapshotAt) >= l.SnapshotInterval
}

// newlyPicked は前回の picked に無かったぶんだけを返す。周期の内側で書くのはこれだけ
// —— 全ランキングを書き直すと、周期を分離した意味が消える。
func newlyPicked(prev map[armSlot]bool, picks []strategy.Candidate) []strategy.Candidate {
	out := make([]strategy.Candidate, 0, len(picks))
	for _, c := range picks {
		if !prev[armSlot{Symbol: c.Symbol, Strategy: c.Strategy}] {
			out = append(out, c)
		}
	}
	return out
}

func (l *AdvisorLoop) markSnapshot(now time.Time, picks []strategy.Candidate) {
	l.lastSnapshotAt = now
	l.lastPicked = pickedSet(picks)
}

func pickedSet(picks []strategy.Candidate) map[armSlot]bool {
	m := make(map[armSlot]bool, len(picks))
	for _, c := range picks {
		m[armSlot{Symbol: c.Symbol, Strategy: c.Strategy}] = true
	}
	return m
}

func samePickedSet(prev map[armSlot]bool, picks []strategy.Candidate) bool {
	if len(prev) != len(picks) {
		return false
	}
	for _, c := range picks {
		if !prev[armSlot{Symbol: c.Symbol, Strategy: c.Strategy}] {
			return false
		}
	}
	return true
}

// best-effort: 監査・検定用途なので、書込失敗はログのみでラウンドを止めない。
func (l *AdvisorLoop) persistScreenSnapshots(ctx context.Context, now time.Time, ranked, picks []strategy.Candidate) {
	if l.Screens == nil {
		return
	}
	pickedKey := make(map[string]bool, len(picks))
	for _, p := range picks {
		pickedKey[p.Symbol+"|"+string(p.Strategy)] = true
	}
	rows := make([]port.ScreenSnapshot, 0, len(ranked))
	for _, c := range ranked {
		rows = append(rows, port.ScreenSnapshot{
			RoundAt: now, Symbol: c.Symbol, Strategy: string(c.Strategy),
			Triggered: c.Triggered, Score: c.Score,
			Picked: pickedKey[c.Symbol+"|"+string(c.Strategy)],
			Side:   string(c.Side),
		})
	}
	if err := l.Screens.InsertRound(ctx, rows); err != nil {
		l.log("advisor_screen_snapshot_persist_failed", "rows", len(rows), "err", err.Error())
	}
}

// この スキャンで LLM を呼ぶ理由を返す(空 = 呼ばない)。優先順は
// pre_open/session_open(その日の1回目)→ new_trigger(立ち上がりのみ)→ heartbeat。
// 1回目を日付境界で撃つのは、固定の毎時 ticker だと起動から約1時間、寄りを逃すため。
// 副作用として prevTriggered / advisedDay を更新する。
func (l *AdvisorLoop) shouldAdvise(now time.Time, ranked []strategy.Candidate) string {
	tz := l.Hours.TZ
	if tz == nil {
		tz = time.UTC
	}
	day := now.In(tz).Format("2006-01-02")

	cur := make(map[string]bool, len(ranked))
	newTrigger := ""
	for _, c := range ranked {
		if !c.Triggered {
			continue
		}
		key := c.Symbol + "|" + string(c.Strategy)
		cur[key] = true
		if !l.prevTriggered[key] && newTrigger == "" {
			newTrigger = c.Symbol
		}
	}
	prevDay := l.advisedDay
	l.prevTriggered = cur
	l.advisedDay = day

	switch {
	case prevDay != day:
		// 寄り前窓なら前倒し、無ければ最初の場中スキャン。どちらも1日1回。
		if l.Hours.InPreOpenWindow(now, l.PreOpenLead) {
			return "pre_open"
		}
		return "session_open"
	case newTrigger != "":
		return "new_trigger:" + newTrigger
	default:
		hb := l.Heartbeat
		if hb <= 0 {
			hb = defaultHeartbeat
		}
		if l.lastAdviseAt.IsZero() || now.Sub(l.lastAdviseAt) >= hb-heartbeatSlack(hb) {
			return "heartbeat"
		}
	}
	return ""
}

// 建玉中の (銘柄, 戦略) は disarm しない(建玉は entry 時に凍結した config で走っている)。
func (l *AdvisorLoop) disarmStale(ctx context.Context, armedNow map[armSlot]bool) {
	if l.Disarm != nil {
		for slot := range l.armedPrev {
			if armedNow[slot] {
				continue
			}
			if l.IsHeld != nil && l.IsHeld(ctx, slot.Symbol, slot.Strategy) {
				continue
			}
			if err := l.Disarm(slot.Symbol, slot.Strategy); err != nil {
				l.log("advisor_loop_disarm_failed", "symbol", slot.Symbol, "strategy", string(slot.Strategy), "err", err.Error())
				continue
			}
			l.log("advisor_loop_disarmed", "symbol", slot.Symbol, "strategy", string(slot.Strategy))
		}
	}
	l.armedPrev = armedNow
}

// arm ゼロ単発は異常ではない(凪 → 全部 no_trade)。連続したときだけ
// 「advisor が壊れている/利用上限」の可能性として Error に上げる。
// 🛑 数えるのは「**候補はあったのに 1 つも arm できなかった**」ラウンドだけ。
// 発火ゼロで arm ゼロは研究モードでは普通に起きる正常状態で、これを異常として
// 鳴らすと静かな日のたびに Error が飛び、警報そのものが無視されるようになる
// (最上位銘柄の強制 arm を外したので、候補ゼロのラウンドが現実に発生する)。
// 候補ゼロは「advisor が健全か」の証拠にならないので、streak は増やしも減らしもしない。
func (l *AdvisorLoop) trackStreak(ctx context.Context, armedAny, hadCandidates bool) {
	if armedAny {
		l.failStreak = 0
		return
	}
	if !hadCandidates {
		return
	}
	l.failStreak++
	if l.failStreak == failStreakAlertThreshold && l.Notifier != nil {
		// 🛑 「consecutive **ticks**」とは言わない。候補ゼロのラウンドは数えも
		// リセットもしないので、間に静かな時間が挟まれば実時間は連続していない
		// (朝の 2 連に午後の 1 件が足されて「3 ティック連続」と鳴る)。
		// **数えているものをそのまま書く。**
		_ = l.Notifier.Notify(ctx, "error", "advisor_loop_no_arm_streak",
			fmt.Sprintf("advisor armed nothing on %d consecutive rounds that had candidates — check advisor health/quota", l.failStreak))
	}
}

// Advisory に決定論パケット(スクリーナー + 直近終値での OCO 幾何)を丸ごと載せる —
// LLM に bot と**同じ**エントリーゲートを見せるため。
func (l *AdvisorLoop) summaryFor(sym string, daily []market.Candle, now time.Time, slot config.StrategyName) *market.MarketSummary {
	asof := ""
	last := 0.0
	if n := len(daily); n > 0 {
		asof = daily[n-1].OpenTime.Format("2006-01-02")
		last = daily[n-1].Close
	}
	raw, _ := json.Marshal(advisor.Build(sym, daily, asof, 0))
	return &market.MarketSummary{
		Symbol:      sym,
		GeneratedAt: now,
		TickSize:    market.TickSizeOf(sym, last),
		CurrentRate: market.CurrentRate{Last: last, At: now},
		Advisory:    raw,
		// プロンプト側の指示に使う(強制は Promoter 側の hard gate)。
		SlotStrategy: string(slot),
	}
}

func (l *AdvisorLoop) universe(ctx context.Context) map[string][]market.Candle {
	u := make(map[string][]market.Candle, len(l.Symbols))
	unaffordable := 0
	for _, sym := range l.Symbols {
		cs, err := l.Candles.List(ctx, sym, port.PeriodDaily, selectorCandleLookback)
		if err != nil || len(cs) < 26 {
			continue
		}
		if !l.affordableLot(cs) {
			unaffordable++
			continue
		}
		u[sym] = cs
	}
	if unaffordable > 0 {
		// 黙って減らさない(「トリガーが減った」と「ユニバースが縮んだ」の取り違え防止)。
		l.log("advisor_universe_notional_filtered", "excluded", unaffordable,
			"kept", len(u), "cap_jpy", l.MaxPositionNotionalJPY, "lot_shares", minLotShares)
	}
	return u
}

// 東証の売買単位。2018年の統一以降、内国普通株式は全銘柄 100 株。
// 例外が出たら、ここを銘柄別テーブルに差し替える。
const minLotShares = 100

// 「そもそも1単元でも買えるか」。cfg.Risk.Quantity ではなく minLotShares で見るのは、
// この判定が config 生成の**前**(LLM に掛ける前)に走るため。
func (l *AdvisorLoop) affordableLot(daily []market.Candle) bool {
	return lotAffordable(l.MaxPositionNotionalJPY, daily)
}

// lotAffordable は advisor の universe と PaperArmCandidates が共有する篩。
func lotAffordable(maxNotionalJPY int, daily []market.Candle) bool {
	if maxNotionalJPY <= 0 {
		return true
	}
	// 終値 0(売買停止・気配のみ)も価格不明 = fail-close。0 は 0*100<=cap で true になり、
	// 価格を検証できないまま銘柄を残す方向に倒れる。
	if len(daily) == 0 || daily[len(daily)-1].Close <= 0 {
		return false
	}
	return daily[len(daily)-1].Close*minLotShares <= float64(maxNotionalJPY)
}

// 第二の関門: affordableLot は1単元ぶんしか保証しない。LLM はより大きい株数を
// 返せる(hard_limits は 3000 株まで許す)ので、9,000円の銘柄が 270万円の建玉を
// 提案しうる。no_trade は対象外(弾くと「なぜ見送ったか」の記録が消える)。
func (l *AdvisorLoop) withinNotionalCap(cfg *config.StrategyConfig, daily []market.Candle) error {
	if l.MaxPositionNotionalJPY <= 0 || cfg == nil || cfg.StrategyName == config.StrategyNoTrade {
		return nil
	}
	// 終値 0 も価格不明。0 だと notional=0 で「上限内」になり、株数によらず arm される。
	if len(daily) == 0 || daily[len(daily)-1].Close <= 0 {
		return fmt.Errorf("notional cap: %s の直近価格が無く建玉金額を検証できない(fail-close)", cfg.Symbol)
	}
	notional := daily[len(daily)-1].Close * float64(cfg.Risk.Quantity)
	if notional > float64(l.MaxPositionNotionalJPY) {
		return fmt.Errorf("notional cap: %s の建玉金額 %.0f円(%d株)が上限 %d円を超える",
			cfg.Symbol, notional, cfg.Risk.Quantity, l.MaxPositionNotionalJPY)
	}
	return nil
}

func (l *AdvisorLoop) screeners() []strategy.Screener {
	if l.Screeners != nil {
		return l.Screeners
	}
	return strategy.DefaultScreeners()
}

func (l *AdvisorLoop) now() time.Time {
	if l.Clock != nil {
		return l.Clock()
	}
	return time.Now()
}

func (l *AdvisorLoop) log(msg string, kv ...any) {
	if l.Logger != nil {
		l.Logger.Info(msg, kv...)
	}
}

// debug は「起きてはいるが毎分見る必要が無い」ものの置き場。**捨てない** —
// 静かなラウンドを Info から落とすだけで、-v なら全部追える。
func (l *AdvisorLoop) debug(msg string, kv ...any) {
	if l.Logger != nil {
		l.Logger.Debug(msg, kv...)
	}
}
