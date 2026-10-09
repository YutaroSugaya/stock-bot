package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"log/slog"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// stubAdvisor returns a fixed run and captures the summary it was given.
type stubAdvisor struct {
	run     *port.AdvisorRun
	gotSumm *market.MarketSummary
}

func (s *stubAdvisor) Generate(_ context.Context, summ *market.MarketSummary) (*port.AdvisorRun, error) {
	s.gotSumm = summ
	return s.run, nil
}

const advisorLoopYAML = `config_id: t1
symbol: "7203"
strategy_name: bnf_reversion
mode: paper_config
holding_mode: multiday
exec_kind: cash
entry:
  direction: buy_only
  max_spread_ticks: 5
exit:
  take_profit_jpy: 0
  stop_loss_jpy: 0
risk:
  quantity: 100
  max_open_positions: 1
`

func tokyoTradingHours() session.TradingHours {
	return session.TradingHours{
		TZ:          clock.JST,
		Sessions:    []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:30"}},
		EntryCutoff: "14:55",
		ForceFlatAt: "14:50",
	}
}

// panicSeries builds n daily candles ending in a BNF panic (last close ~-15%
// below a flat 25-MA on high volume) so RankCandidates surfaces the symbol.
func panicSeries(sym string) []market.Candle {
	cs := make([]market.Candle, 0, 40)
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 39; i++ {
		cs = append(cs, market.Candle{Symbol: sym, OpenTime: t0.AddDate(0, 0, i),
			Open: 1000, High: 1000, Low: 1000, Close: 1000, Volume: 1000})
	}
	cs = append(cs, market.Candle{Symbol: sym, OpenTime: t0.AddDate(0, 0, 39),
		Open: 850, High: 850, Low: 850, Close: 850, Volume: 3000})
	return cs
}

// **スクリーナーを BNF 1 本に絞った**。
//
// screener が**売り側でも Triggered を立てる**ので、`panicSeries`
// (急落)は BNF 2 アームだけでなく **donchian の下方ブレイク**(v2 とその兄弟)も
// 成立させる。既定の全スクリーナーのままだと 1 ラウンドで 4 枠 = LLM 4 回になり、
// **「1 ラウンドが何回発火したか」**を見ているテスト群(quiet ゲート / 寄り前 1 回 /
// 手動)の主張が「枠が何個配られたか」にすり替わる。
//
// ここで測りたいのは**ラウンドの発火条件**なので、スクリーナーを 1 本に固定して
// 「1 ラウンド = 1 呼び出し」を保つ。全スクリーナーぶんの挙動は
// TestAdvisorLoop_PersistsScreenSnapshots と TestAdvisorLoop_ArmsAllTriggeredUpToTopN が別に見る。
func newLoop(t *testing.T, adv port.Advisor, armed *[]*config.StrategyConfig, clockAt time.Time) *AdvisorLoop {
	t.Helper()
	l := newLoopWithScreeners(t, adv, armed, clockAt)
	l.Screeners = []strategy.Screener{strategy.BNFReversion{}}
	return l
}

// newLoopWithScreeners は既定の全スクリーナーで回す版(スナップショットの検証用)。
func newLoopWithScreeners(t *testing.T, adv port.Advisor, armed *[]*config.StrategyConfig, clockAt time.Time) *AdvisorLoop {
	t.Helper()
	repo := repository.NewInMemoryCandleRepo()
	if err := repo.Upsert(context.Background(), "7203", panicSeries("7203")); err != nil {
		t.Fatal(err)
	}
	return oneSymbolLoop(repo, adv, armed, clockAt, 0)
}

// テストの hard_limits。値域は「全部通す」広さ — このパッケージのテストは
// hard_limits の境界ではなく advisor loop の挙動を見る。
func testHardLimits(syms ...string) *config.HardLimits {
	return &config.HardLimits{
		AllowedSymbols: syms,
		Quantity:       config.IntRange{Min: 1, Max: 1000},
		TakeProfitJPY:  config.FloatRange{Min: 1, Max: 1000},
		StopLossJPY:    config.FloatRange{Min: 1, Max: 1000},
	}
}

// armLoop は arm を記録する AdvisorLoop。銘柄数と枠以外は共通なので、テストごとに
// 18 行のリテラルを組み立て直さない。TopN=0 は「上限を指定しない」既定のまま。
// 🛑 perStrategyN は既定値を持たせず**必ず明示引数**にする(ゼロ値をクォータ無効に
// 倒すと、枠の検証をしているテストが黙って別物を測る)。
func armLoop(repo port.CandleRepository, syms []string, adv port.Advisor, armed *[]*config.StrategyConfig, at time.Time, topN, perStrategyN int) *AdvisorLoop {
	return &AdvisorLoop{
		Symbols:    syms,
		Candles:    repo,
		Advisor:    adv,
		HardLimits: testHardLimits(syms...),
		Hours:      tokyoTradingHours(),
		Clock:      func() time.Time { return at },
		Arm: func(c *config.StrategyConfig) error {
			*armed = append(*armed, c)
			return nil
		},
		TopN:         topN,
		PerStrategyN: perStrategyN,
	}
}

// oneSymbolLoop は 7203 だけを見る loop。
func oneSymbolLoop(repo port.CandleRepository, adv port.Advisor, armed *[]*config.StrategyConfig, at time.Time, topN int) *AdvisorLoop {
	return armLoop(repo, []string{"7203"}, adv, armed, at, topN, 0)
}

func TestAdvisorLoop_ArmsInSession(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST) // Tue 10:00 JST
	adv := &stubAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	newLoop(t, adv, &armed, inSession).Tick(context.Background())

	if len(armed) != 1 || armed[0].Symbol != "7203" || armed[0].StrategyName != config.StrategyBNFReversion {
		t.Fatalf("want 7203 bnf_reversion armed, got %+v", armed)
	}
	// enrichment: the LLM must have received the deterministic packet
	if adv.gotSumm == nil || len(adv.gotSumm.Advisory) == 0 {
		t.Fatalf("summary Advisory packet not populated: %+v", adv.gotSumm)
	}
}

func TestAdvisorLoop_SkipsOutOfSession(t *testing.T) {
	afterClose := time.Date(2026, 7, 21, 16, 0, 0, 0, clock.JST) // after 15:30
	adv := &stubAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	newLoop(t, adv, &armed, afterClose).Tick(context.Background())

	if len(armed) != 0 {
		t.Fatalf("out of session must arm nothing, got %+v", armed)
	}
	if adv.gotSumm != nil {
		t.Fatal("advisor must not be called out of session")
	}
}

// countingAdvisor records how many times the LLM was actually invoked.
type countingAdvisor struct {
	run    *port.AdvisorRun
	callsN int
}

func (c *countingAdvisor) Generate(_ context.Context, _ *market.MarketSummary) (*port.AdvisorRun, error) {
	c.callsN++
	return c.run, nil
}

// The scan is cheap and runs every minute, but the LLM must NOT be called on
// every scan: after the session-open fire, a still-triggered candidate must not
// re-fire until the heartbeat elapses.
func TestAdvisorLoop_DoesNotCallLLMOnEveryScan(t *testing.T) {
	at := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	loop := newLoop(t, adv, &armed, at)
	loop.Heartbeat = time.Hour
	now := at
	loop.Clock = func() time.Time { return now }

	loop.Tick(context.Background()) // session_open → 1 call
	for i := 0; i < 5; i++ {        // 5 more scans, same state → no new calls
		now = now.Add(time.Minute)
		loop.Tick(context.Background())
	}
	if adv.callsN != 1 {
		t.Fatalf("LLM should run once (session_open), got %d calls", adv.callsN)
	}
}

// A candidate that becomes triggered mid-session must fire the LLM IMMEDIATELY
// (rising edge) — not wait for the hourly heartbeat.
func TestAdvisorLoop_NewTriggerFiresImmediately(t *testing.T) {
	at := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	loop := newLoop(t, adv, &armed, at)
	loop.Heartbeat = time.Hour
	now := at
	loop.Clock = func() time.Time { return now }

	// Pretend the session-open fire already happened today with NOTHING triggered,
	// so the panic below is a genuine rising edge.
	loop.advisedDay = at.Format("2006-01-02")
	loop.prevTriggered = map[string]bool{}
	loop.lastAdviseAt = at

	now = at.Add(3 * time.Minute) // well inside the heartbeat window
	loop.Tick(context.Background())

	if adv.callsN != 1 {
		t.Fatalf("a new trigger must fire the LLM immediately, got %d calls", adv.callsN)
	}
}

// Out of session the scan must never call the LLM.
// LLM の自動発火は「取引日の pre-open 窓+場中」以外は絶対に無い(週末・夜間に
// claude CLI がトークンを消費しない)。本番 config が使う
// PreOpenLead=15分でも、土曜・窓の外・引け後は発火しないことを固定する。
func TestAdvisorLoop_NoLLMOutOfSession(t *testing.T) {
	for name, tc := range map[string]struct {
		at   time.Time
		lead time.Duration
	}{
		"weekday_after_close":           {at: time.Date(2026, 7, 21, 16, 0, 0, 0, clock.JST)},
		"saturday_in_hours":             {at: time.Date(2026, 7, 25, 10, 0, 0, 0, clock.JST)},
		"saturday_preopen_with_lead":    {at: time.Date(2026, 7, 25, 8, 50, 0, 0, clock.JST), lead: 15 * time.Minute},
		"weekday_before_window":         {at: time.Date(2026, 7, 21, 8, 30, 0, 0, clock.JST), lead: 15 * time.Minute},
		"weekday_after_close_with_lead": {at: time.Date(2026, 7, 21, 16, 0, 0, 0, clock.JST), lead: 15 * time.Minute},
	} {
		adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
		var armed []*config.StrategyConfig
		loop := newLoop(t, adv, &armed, tc.at)
		loop.PreOpenLead = tc.lead
		loop.Tick(context.Background())
		if adv.callsN != 0 {
			t.Fatalf("%s: out of session must not call the LLM, got %d", name, adv.callsN)
		}
	}
}

// lead>0 のとき、寄り前窓内の手動実行はその日の1回目を兼ねる(状態を保持)—
// 同一データのまま 09:00 に自動ラウンドを重ねない(トークンの二重払い防止)。
func TestAdvisorLoop_ManualInPreOpenWindowCountsAsDailyRound(t *testing.T) {
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	now := time.Date(2026, 7, 21, 8, 50, 0, 0, clock.JST)
	loop := newLoop(t, adv, &armed, now)
	loop.PreOpenLead = 15 * time.Minute
	loop.Clock = func() time.Time { return now }

	loop.adviseManual(context.Background())
	if adv.callsN != 1 {
		t.Fatalf("窓内の手動が動かない: callsN=%d", adv.callsN)
	}
	now = time.Date(2026, 7, 21, 9, 0, 30, 0, clock.JST)
	loop.Tick(context.Background())
	if adv.callsN != 1 {
		t.Fatalf("窓内手動が日次1回目を兼ねていない(自動が重なった): callsN=%d", adv.callsN)
	}
}

// 寄り前ウォームアップ(PreOpenLead > 0): 取引日の寄り前 lead 分に入ったら、
// その日の最初の LLM ラウンドを前倒しで回して arm 済みにする。LLM の入力は
// 前日確定足なので 9:00 に回すのと判断材料は同一 — 寄り直後の初動を
// LLM レイテンシ(最大 timeout_seconds)で取り逃さないための前倒し。
func TestAdvisorLoop_PreOpenAdvisesOnceAndReplacesSessionOpen(t *testing.T) {
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	now := time.Date(2026, 7, 21, 8, 50, 0, 0, clock.JST) // 火曜 寄り前10分
	loop := newLoop(t, adv, &armed, now)
	loop.PreOpenLead = 15 * time.Minute
	loop.Clock = func() time.Time { return now }

	loop.Tick(context.Background()) // 寄り前窓: pre_open で発火
	if adv.callsN != 1 || len(armed) != 1 {
		t.Fatalf("寄り前に LLM が回っていない: callsN=%d armed=%d", adv.callsN, len(armed))
	}
	loop.Tick(context.Background()) // 同じ寄り前窓の次スキャン: 二重発火しない
	if adv.callsN != 1 {
		t.Fatalf("寄り前に毎分発火している(1日1回のはず): callsN=%d", adv.callsN)
	}
	now = time.Date(2026, 7, 21, 9, 0, 30, 0, clock.JST)
	loop.Tick(context.Background()) // 寄り付き: pre_open が済んでいるので session_open は重複しない
	if adv.callsN != 1 {
		t.Fatalf("pre_open 済みなのに session_open が二重発火: callsN=%d", adv.callsN)
	}
}

// PreOpenLead 未設定(既定0)は従来どおり: 寄り前は発火しない(後方互換)。
func TestAdvisorLoop_NoPreOpenByDefault(t *testing.T) {
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	newLoop(t, adv, &armed, time.Date(2026, 7, 21, 8, 50, 0, 0, clock.JST)).Tick(context.Background())
	if adv.callsN != 0 {
		t.Fatalf("PreOpenLead=0 で寄り前に発火した: callsN=%d", adv.callsN)
	}
}

// 手動トリガー(dashboard のボタン・以前の別プロジェクトの手動 advisor 相当)は quiet ゲート
// (rising-edge/heartbeat)をバイパスして必ず 1 回 LLM を呼ぶ。
func TestAdvisorLoop_ManualAdviseBypassesQuietGates(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	loop := newLoop(t, adv, &armed, inSession)
	loop.Tick(context.Background()) // session_open で 1 回目
	loop.Tick(context.Background()) // 静か: rising-edge なし・heartbeat 未達 → 呼ばれない
	if adv.callsN != 1 {
		t.Fatalf("前提が崩れている(quiet ゲート): callsN=%d, want 1", adv.callsN)
	}
	loop.adviseManual(context.Background())
	if adv.callsN != 2 {
		t.Fatalf("manual advise が LLM を呼んでいない: callsN=%d, want 2", adv.callsN)
	}
}

// 手動は場外でも動く(人間の観測用 — 自動発火の「平日場中のみ」は NoLLMOutOfSession
// が守り続ける)。場外で arm されてもエントリーは決定論 risk gate が場中しか許さない。
func TestAdvisorLoop_ManualAdviseWorksOutOfSession(t *testing.T) {
	saturday := time.Date(2026, 7, 25, 10, 0, 0, 0, clock.JST)
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	loop := newLoop(t, adv, &armed, saturday)
	loop.adviseManual(context.Background())
	if adv.callsN != 1 {
		t.Fatalf("手動が場外で動かない(観測用なので動くべき): callsN=%d", adv.callsN)
	}
}

// 単一飛行: advise 実行中(ロック保持中)の StartManual は busy を返し重ねない。
// LLM は最長 timeout_seconds 走るので、二重起動はトークンの二重払いになる。
func TestAdvisorLoop_StartManualSingleFlight(t *testing.T) {
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	loop := newLoop(t, adv, &armed, time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST))
	loop.mu.Lock()
	err := loop.StartManual(context.Background())
	loop.mu.Unlock()
	if err == nil {
		t.Fatal("実行中の二重起動を拒否していない(ErrAdviseBusy を返すべき)")
	}
}

// StartManual の成功経路: goroutine が実際に advise を走らせ、完了後にロックが
// 解放される(= Tick が再開でき、ボタンが恒久 409 にならない)。mu.Lock は
// goroutine 完了までブロックするので決定論の完了待ちになり、callsN の読みも
// happens-before が成立する(-race 下でこの並行経路を実際に踏む)。
func TestAdvisorLoop_StartManualRunsAndReleasesLock(t *testing.T) {
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	loop := newLoop(t, adv, &armed, time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST))
	if err := loop.StartManual(context.Background()); err != nil {
		t.Fatalf("StartManual: %v", err)
	}
	loop.mu.Lock() // 完了待ち。取得できた = goroutine が Unlock した証明
	defer loop.mu.Unlock()
	if adv.callsN != 1 {
		t.Fatalf("goroutine が advise を走らせていない: callsN=%d", adv.callsN)
	}
}

// 寄り前(場外)の手動実行が当日の session_open 発火を消費しない。手動ラウンドが
// 失敗しても 09:00 の自動ラウンド(その日最初の LLM)は従来どおり走る。
func TestAdvisorLoop_ManualPreOpenDoesNotEatSessionOpen(t *testing.T) {
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	now := time.Date(2026, 7, 21, 8, 30, 0, 0, clock.JST) // 火曜 寄り前
	loop := newLoop(t, adv, &armed, now)
	loop.Clock = func() time.Time { return now }

	loop.adviseManual(context.Background())
	if adv.callsN != 1 {
		t.Fatalf("寄り前の手動が動かない: callsN=%d", adv.callsN)
	}
	now = time.Date(2026, 7, 21, 9, 0, 30, 0, clock.JST) // 寄り付き後の最初のスキャン
	loop.Tick(context.Background())
	if adv.callsN != 2 {
		t.Fatalf("寄り前の手動が session_open を食った: callsN=%d, want 2", adv.callsN)
	}
}

// 手動ラウンドは failStreak に算入しない: 週末に何度試して arm ゼロでも
// 「advisor 異常」の誤アラートを出さない。
func TestAdvisorLoop_ManualDoesNotFeedFailStreak(t *testing.T) {
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunCLIError, ErrorMsg: "quota"}}
	var armed []*config.StrategyConfig
	loop := newLoop(t, adv, &armed, time.Date(2026, 7, 25, 10, 0, 0, 0, clock.JST)) // 土曜
	for i := 0; i < 3; i++ {
		loop.adviseManual(context.Background())
	}
	if loop.failStreak != 0 {
		t.Fatalf("手動ラウンドが failStreak を汚した: %d", loop.failStreak)
	}
}

// perSymbolAdvisor generates a valid config for whichever symbol it is asked
// about(複数 arm テスト用 — 固定 YAML だと2銘柄目が symbol 不一致で reject される)。
// 枠の戦略拘束に従い、strategy_name には slot_strategy をそのまま使う
// (実 LLM の契約と同じ。枠違いは promote が reject する)。
type perSymbolAdvisor struct{ calls int }

func (a *perSymbolAdvisor) Generate(_ context.Context, summ *market.MarketSummary) (*port.AdvisorRun, error) {
	a.calls++
	slot := summ.SlotStrategy
	if slot == "" {
		slot = "bnf_reversion"
	}
	y := fmt.Sprintf(`config_id: t-%s
symbol: "%s"
strategy_name: %s
mode: paper_config
holding_mode: multiday
exec_kind: cash
entry:
  direction: buy_only
  max_spread_ticks: 5
exit:
  take_profit_jpy: 100
  stop_loss_jpy: 50
risk:
  quantity: 100
  max_open_positions: 1
`, summ.Symbol, summ.Symbol, slot)
	return &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(y)}, nil
}

func twoSymbolLoop(t *testing.T, adv port.Advisor, armed *[]*config.StrategyConfig, clockAt time.Time, topN int) *AdvisorLoop {
	t.Helper()
	repo := repository.NewInMemoryCandleRepo()
	for _, sym := range []string{"7203", "6758"} {
		if err := repo.Upsert(context.Background(), sym, panicSeries(sym)); err != nil {
			t.Fatal(err)
		}
	}
	// 同一戦略の2銘柄を使う fixture なので、戦略別クォータでは絞らない
	// (このテストの主題は「トリガーを取りこぼさないこと」)。
	return armLoop(repo, []string{"7203", "6758"}, adv, armed, clockAt, topN, topN)
}

// 研究モード: トリガー成立した候補は上限まで
// 「全部」advise・arm する。1銘柄に絞ると forward サンプルが痩せる。
// 上限は全体の TopN と**戦略あたりの PerStrategyN** の両方(枠を戦略に配るのは
// スコアが戦略間で比較不能なため — selectAdvisePicks 参照)。
//
// 枠は **(銘柄, 戦略)** 単位。fixture の panicSeries は 2 銘柄それぞれで
// bnf_reversion と bnf_reversion_trail の両方をトリガーするので、**2 銘柄 × 2 アーム = 4**
// が正しい。以前ここが 2 だったのは同一銘柄を 1 回に畳んでいたからで、それが
// ペア標本が 0 本になる原因そのもの。
func TestAdvisorLoop_ArmsAllTriggeredUpToTopN(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &perSymbolAdvisor{}
	var armed []*config.StrategyConfig
	// 枠は 20。screener が売り側でも Triggered を
	// 立てるので、`panicSeries` は BNF 2 アーム + **donchian の下方ブレイク**
	// 2 アーム = 1 銘柄 4 アームを成立させる。5 枠だと**全体キャップが拘束して**
	// 「トリガー全採用」を見るテストが cap の検証にすり替わる。
	twoSymbolLoop(t, adv, &armed, inSession, 20).Tick(context.Background())
	if adv.calls != len(armed) {
		t.Fatalf("生成回数と arm 数が合わない: llm_calls=%d armed=%d", adv.calls, len(armed))
	}
	// bnf_stabilized_reversion(+ _trail)の screener は bnf の入口を完全に
	// ミラーするので、panicSeries は 1 銘柄 **6 アーム**を成立させる(bnf ×2 + stabilized ×2 +
	// donchian の売り ×2)。day2 は「前日はまだ −12% 未達」が条件なので成立しない。
	// bnf_intraday_reversion(+ _trail)も日足の screener は bnf をミラーするので
	// **8 アーム**(bnf ×2 + stabilized ×2 + intraday ×2 + donchian の売り ×2)。
	if len(armed) != 16 {
		t.Fatalf("トリガー全採用になっていない: armed=%d (2銘柄 × 8アーム = 16)", len(armed))
	}
	arms := map[config.StrategyName]bool{}
	for _, c := range armed {
		arms[c.StrategyName] = true
	}
	if len(arms) != 8 {
		t.Fatalf("成立している 8 アームが揃っていない: %+v", arms)
	}
	syms := map[string]bool{}
	for _, c := range armed {
		syms[c.Symbol] = true
	}
	if !syms["7203"] || !syms["6758"] {
		t.Fatalf("両銘柄が arm されていない: %+v", syms)
	}
}

// 保有中銘柄は TopN の枠を消費しない(レビュー M1): 上位が既保有でも、
// 次の新規トリガーがちゃんと advise される。
func TestAdvisorLoop_HeldSymbolDoesNotConsumeTriggerSlot(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &perSymbolAdvisor{}
	var armed []*config.StrategyConfig
	loop := twoSymbolLoop(t, adv, &armed, inSession, 1) // 枠は1
	// 同点スコアのタイブレークで 6758 が最上位 — それを保有中にする。
	loop.IsHeld = func(_ context.Context, sym string, _ config.StrategyName) bool { return sym == "6758" }
	loop.Tick(context.Background())
	if adv.calls != 1 || len(armed) != 1 || armed[0].Symbol != "7203" {
		t.Fatalf("保有銘柄が枠を食い潰している: calls=%d armed=%+v (want 7203 が枠を得る)", adv.calls, armed)
	}
}

// risingSeries: 260本の単調上昇 = 52週高値・ドンチャン・絶対モメンタムの3スクリーナーが
// 同時にトリガーする(dedup 検証用)。
func risingSeries(sym string, n int) []market.Candle {
	cs := make([]market.Candle, 0, n)
	t0 := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		px := 100 + float64(i)*0.5
		cs = append(cs, market.Candle{Symbol: sym, OpenTime: t0.AddDate(0, 0, i),
			Open: px, High: px, Low: px, Close: px, Volume: 1000})
	}
	return cs
}

// **契約が反転した**。以前ここは「同一銘柄が複数スクリーナーでトリガーしても
// arm は 1 回」を固定していた(1銘柄1 config だった頃の形)。建玉の一意性キーが
// (銘柄, 戦略) になったので、**戦略が違えば同じ銘柄が複数 arm される**のが正しい —
// 潰していたことが、入口が同一で出口だけ違う 2 アームのペア標本が 1 本も成立していなかった
// 原因そのものだった。dedup が残っているのは **同一 (銘柄, 戦略)** の重複だけ。
func TestAdvisorLoop_ArmsOnePerStrategyForAMultiTriggerSymbol(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	repo := repository.NewInMemoryCandleRepo()
	if err := repo.Upsert(context.Background(), "7203", risingSeries("7203", 260)); err != nil {
		t.Fatal(err)
	}
	adv := &perSymbolAdvisor{}
	var armed []*config.StrategyConfig
	loop := oneSymbolLoop(repo, adv, &armed, inSession, 5)
	loop.Tick(context.Background())

	if len(armed) < 2 {
		t.Fatalf("複数戦略でトリガーした銘柄が %d 本しか arm されていない", len(armed))
	}
	if adv.calls != len(armed) {
		t.Fatalf("生成回数と arm 数が合わない: calls=%d armed=%d", adv.calls, len(armed))
	}
	seen := map[config.StrategyName]bool{}
	for _, c := range armed {
		if c.Symbol != "7203" {
			t.Fatalf("別銘柄が混ざった: %+v", c)
		}
		if seen[c.StrategyName] {
			t.Fatalf("同一 (銘柄, 戦略) が二重に arm された: %q", c.StrategyName)
		}
		seen[c.StrategyName] = true
	}
}

// 枠の戦略拘束: per-strategy round-robin が配った枠の戦略
// (SlotStrategy)をパケットに刻み、LLM が枠と違う戦略の config を返しても
// arm しない。これが無いと包含関係の戦略(52週高値 ⊂ abs_momentum)は LLM の
// 選好で毎回横取りされ、標本が永久にゼロになる(実測: 285 run 中 0 回)。
func TestAdvisorLoop_BindsSlotStrategy(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	repo := repository.NewInMemoryCandleRepo()
	// rising series は high_52w / donchian / abs_momentum が同時トリガー —
	// 枠はローテーションでどれか1つに決まる。LLM 側はトリガー外の bnf_reversion
	// を返す(メニュー内だが枠違い)。
	if err := repo.Upsert(context.Background(), "7203", risingSeries("7203", 260)); err != nil {
		t.Fatal(err)
	}
	adv := &stubAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	loop := oneSymbolLoop(repo, adv, &armed, inSession, 1)
	loop.Tick(context.Background())
	if adv.gotSumm == nil || adv.gotSumm.SlotStrategy == "" {
		t.Fatalf("SlotStrategy がパケットに刻まれていない: %+v", adv.gotSumm)
	}
	if len(armed) != 0 {
		t.Fatalf("枠(%s)と違う戦略 bnf_reversion が arm された: %+v", adv.gotSumm.SlotStrategy, armed)
	}
}

// migration 0008: LLM ラウンドごとに全銘柄 × 全スクリーナーの
// screen 結果(triggered / score / picked)を丸ごと永続化する。advisor_runs には
// 選ばれた銘柄しか残らないため、「スコアは高かったが枠に入らなかった銘柄の
// その後」= 枠配分ロジック自体の検証が、これ無しでは原理的に不可能。
func TestAdvisorLoop_PersistsScreenSnapshots(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &perSymbolAdvisor{}
	var armed []*config.StrategyConfig
	loop := newLoopWithScreeners(t, adv, &armed, inSession) // 全スクリーナーぶんの行を見る
	snaps := repository.NewInMemoryScreenSnapshotRepo()
	loop.Screens = snaps
	loop.Tick(context.Background())

	rows := snaps.All()
	// 1銘柄 × 全スクリーナー。triggered も非 triggered も両方残る。本数は
	// DefaultScreeners から導く — 直値で書くと戦略を足すたびに無関係な失敗が出て、
	// 「スナップショットが全スクリーナーぶん残る」という本来の主張がぼやける。
	want := len(strategy.DefaultScreeners())
	if len(rows) != want {
		t.Fatalf("screen snapshot 行数 = %d, want %d(全スクリーナーぶん)", len(rows), want)
	}
	picked, triggered := 0, 0
	for _, r := range rows {
		if r.Symbol != "7203" || r.RoundAt.IsZero() {
			t.Fatalf("bad row: %+v", r)
		}
		if r.Picked {
			picked++
			if !r.Triggered {
				t.Fatalf("picked なのに triggered でない行: %+v", r)
			}
		}
		if r.Triggered {
			triggered++
		}
	}
	// `panicSeries`(急落)で成立するのは **4 アーム**(bnf_reversion / bnf_reversion_trail /
	// donchian_breakout_v2 / donchian_breakout_v2_trail)。donchian の 2 本は
	// screener が**売り側(20日安値ブレイク)でも Triggered を立てる**ので加わる。
	// picked は newLoop の per_strategy_n=2 と、stub が bnf しか返さない拘束で決まる。
	if triggered != 8 {
		t.Fatalf("triggered=%d, want 8(bnf ×2 + stabilized ×2 + intraday ×2 + donchian の売り ×2・2026-09-11)", triggered)
	}
	if picked == 0 {
		t.Fatalf("picked=%d — 枠が 1 つも配られていない", picked)
	}
}

// snapshot の書込失敗は advise/arm を止めない(監査用途・best-effort)。
func TestAdvisorLoop_ScreenSnapshotFailureDoesNotBlockArm(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &perSymbolAdvisor{}
	var armed []*config.StrategyConfig
	loop := newLoop(t, adv, &armed, inSession)
	loop.Screens = failingScreenSnapshotRepo{}
	loop.Tick(context.Background())
	if len(armed) != 1 {
		t.Fatalf("snapshot 失敗で arm が止まった: armed=%d", len(armed))
	}
}

type failingScreenSnapshotRepo struct{}

func (failingScreenSnapshotRepo) InsertRound(context.Context, []port.ScreenSnapshot) error {
	return fmt.Errorf("db down")
}

// TopN は「トリガー採用の上限」: 1 なら従来どおり最上位1銘柄のみ。
func TestAdvisorLoop_TopNCapsTriggeredArms(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &perSymbolAdvisor{}
	var armed []*config.StrategyConfig
	twoSymbolLoop(t, adv, &armed, inSession, 1).Tick(context.Background())
	if adv.calls != 1 || len(armed) != 1 {
		t.Fatalf("TopN=1 の上限が効いていない: llm_calls=%d armed=%d (want 1/1)", adv.calls, len(armed))
	}
}

type recNotifier struct{ calls []string }

func (r *recNotifier) Notify(_ context.Context, level, title, _ string) error {
	r.calls = append(r.calls, level+":"+title)
	return nil
}

// A failing advisor run must produce an operator alert (not just a log), and a
// streak of no-arm ticks must escalate to an Error.
func TestAdvisorLoop_NotifiesOnFailureAndEscalates(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &stubAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunCLIError, ErrorMsg: "boom"}}
	var armed []*config.StrategyConfig
	loop := newLoop(t, adv, &armed, inSession)
	rn := &recNotifier{}
	loop.Notifier = rn
	// Short heartbeat + advancing clock so each Tick is a real advise round
	// (the streak counts advise rounds, not cheap scans).
	loop.Heartbeat = time.Minute
	now := inSession
	loop.Clock = func() time.Time { return now }

	for i := 0; i < failStreakAlertThreshold; i++ {
		loop.Tick(context.Background())
		now = now.Add(time.Minute)
	}
	if len(armed) != 0 {
		t.Fatalf("failing advisor must arm nothing, got %+v", armed)
	}
	var warns, errs int
	for _, c := range rn.calls {
		if c == "warn:advisor_run_failed" {
			warns++
		}
		if c == "error:advisor_loop_no_arm_streak" {
			errs++
		}
	}
	if warns < failStreakAlertThreshold {
		t.Fatalf("expected a warn per failing tick, got %d (%v)", warns, rn.calls)
	}
	if errs != 1 {
		t.Fatalf("expected exactly one streak escalation, got %d (%v)", errs, rn.calls)
	}
}

// A rejected advisor config (off-menu) arms nothing even in session.
func TestAdvisorLoop_RejectedConfigArmsNothing(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	bad := []byte("config_id: x\nsymbol: \"7203\"\nstrategy_name: time_series_momentum\nmode: paper_config\nrisk:\n  quantity: 100\n")
	adv := &stubAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: bad}}
	var armed []*config.StrategyConfig
	newLoop(t, adv, &armed, inSession).Tick(context.Background())
	if len(armed) != 0 {
		t.Fatalf("off-menu config must arm nothing, got %+v", armed)
	}
}

// A symbol holding a position must never be re-armed (its slot is taken and it
// runs on the config frozen at entry) — the Selector rule the advisor replaced.
func TestAdvisorLoop_SkipsHeldSymbol(t *testing.T) {
	at := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	loop := newLoop(t, adv, &armed, at)
	loop.IsHeld = func(context.Context, string, config.StrategyName) bool { return true }

	loop.Tick(context.Background())
	if len(armed) != 0 {
		t.Fatalf("held symbol must not be re-armed, got %+v", armed)
	}
	if adv.callsN != 0 {
		t.Fatalf("held symbol should not even reach the LLM, got %d calls", adv.callsN)
	}
}

// Symbols armed by a previous round that are no longer picked get disarmed, so
// the armed set does not grow monotonically all session.
func TestAdvisorLoop_DisarmsStaleSymbol(t *testing.T) {
	at := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &countingAdvisor{run: &port.AdvisorRun{Status: port.AdvisorRunSuccess, ParsedYAML: []byte(advisorLoopYAML)}}
	var armed []*config.StrategyConfig
	loop := newLoop(t, adv, &armed, at)
	var disarmed []string
	loop.Disarm = func(sym string, _ config.StrategyName) error { disarmed = append(disarmed, sym); return nil }
	loop.Heartbeat = time.Minute
	now := at
	loop.Clock = func() time.Time { return now }

	loop.Tick(context.Background()) // arms 7203
	if len(armed) != 1 {
		t.Fatalf("setup: expected an arm, got %+v", armed)
	}
	// Next round the advisor returns a rejected (off-menu) config → arms nothing,
	// so the previously armed symbol must be disarmed.
	adv.run = &port.AdvisorRun{Status: port.AdvisorRunSuccess,
		ParsedYAML: []byte("config_id: x\nsymbol: \"7203\"\nstrategy_name: time_series_momentum\nmode: paper_config\nholding_mode: multiday\nrisk:\n  quantity: 100\n")}
	now = at.Add(time.Minute)
	loop.Tick(context.Background())

	if len(disarmed) != 1 || disarmed[0] != "7203" {
		t.Fatalf("stale armed symbol must be disarmed, got %v", disarmed)
	}
}

// 🛑 **売りの発火が台帳に残らないと、後から切り分けられない**。
//
// `direction: both` の 4 戦略は売りも出すが、
// `screen_snapshots` に向きの列が無く、買いと売りが同じ行形で混ざっていた。
// 事前登録は「売りの標本ゼロ」を **一覧が空 / 真に非貸借 / そもそも発火していない**
// の 3 つに切り分けると事前登録している。3 つ目はスキャンの向きが残っていないと
// 原理的に読めない(建玉になった売りは positions.side に残るが、
// 「発火したが建たなかった売り」はどこにも残らない)。
func TestAdvisorLoop_ScreenSnapshotsRecordSide(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &perSymbolAdvisor{}
	var armed []*config.StrategyConfig
	loop := newLoopWithScreeners(t, adv, &armed, inSession)
	snaps := repository.NewInMemoryScreenSnapshotRepo()
	loop.Screens = snaps
	loop.Tick(context.Background())

	rows := snaps.All()
	if len(rows) == 0 {
		t.Fatal("snapshot が 1 行も残っていない")
	}
	// 買い専用スクリーナー(BNF 等)は空のまま。向きを持つ戦略が 1 つでも
	// 値を残していれば、切り分けの材料としては十分。
	var withSide int
	for _, r := range rows {
		switch r.Side {
		case "", "BUY", "SELL":
		default:
			t.Fatalf("Side=%q は BUY / SELL / 空 のいずれでもない: %+v", r.Side, r)
		}
		if r.Side != "" {
			withSide++
		}
	}
	if withSide == 0 {
		t.Error("向きを残した行が 1 つも無い — 売りの発火が台帳から消える")
	}
}

// 🛑 **何も起きないラウンドを毎回ログに出さない**。
//
// 決定論 arm は 1 分ごとに回り、枠が埋まっていると `slots: 0` のまま
// `advisor_loop_advise` + `advisor_loop_generate_begin` を毎回吐く。実測で
// **1 営業日の午前だけで 106 回**。本当に見たいログ(守りの期日訂正の失敗など)が
// 埋もれる。
//
// 🛑 ただし**無音にはしない**。「何も起きていない」と「壊れて回っていない」は
// 別の状態で、後者に気づけなくなるのが最悪。**状態が変わったときだけ出す**
// (枠が配れた / 発火の顔ぶれが変わった)。
func TestAdvisorLoop_QuietWhenNothingChanges(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &perSymbolAdvisor{}
	var armed []*config.StrategyConfig
	loop := newLoopWithScreeners(t, adv, &armed, inSession)

	var lines []string
	loop.Logger = slog.New(capturingHandler{fn: func(msg string) { lines = append(lines, msg) }})

	// heartbeat を 1 分にして、実運用と同じ「毎分 heartbeat で advise が回る」形にする
	// (これが無いと shouldAdvise が "" を返して advise 自体が呼ばれず、
	//  ノイズの再現にならない)。
	loop.Heartbeat = time.Minute

	// 同じ状態で 3 周回す。時計を進めて heartbeat を毎回成立させる。
	loop.Tick(context.Background())
	first := len(lines)
	lines = nil
	for i := 0; i < 3; i++ {
		loop.Clock = func() time.Time { return inSession.Add(time.Duration(i+1) * 2 * time.Minute) }
		loop.Tick(context.Background())
	}

	if first == 0 {
		t.Fatal("1 周目は何か出るはず(初回は状態が変わっている)")
	}
	for _, m := range lines {
		if m == "advisor_loop_generate_begin" {
			t.Errorf("状態が変わっていないのに %q を出している — 1分ごとにこれが出ると本当に見たいログが埋もれる", m)
		}
	}
}

// capturingHandler は slog のメッセージ名だけを拾う最小ハンドラ。
// 🛑 **Info 以上だけを拾う** — 運用者が実際に見る面と同じにする。全レベルを拾うと
// 「Debug へ落とした」という是正がテストから見えず、直したのに落ち続ける。
type capturingHandler struct{ fn func(string) }

func (h capturingHandler) Enabled(_ context.Context, lv slog.Level) bool {
	return lv >= slog.LevelInfo
}
func (h capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.fn(r.Message)
	return nil
}
func (h capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h capturingHandler) WithGroup(string) slog.Handler      { return h }

// 🛑 **場外スキップを毎分出さない**。
//
// 昼休み(11:30〜12:30)と場外は `advisor_loop_skip_out_of_session` が 1 分ごとに出る。
// 昼休みだけで 60 行、1 日では数百行になり、本当に見たいログが埋もれる。
//
// 🛑 無音にはしない。**場外に入った最初の 1 回は Info で出す**(「いま場外だから
// 止まっている」が読めないと、故障で止まっているのと区別が付かない)。
// 2 回目以降は Debug。場中に戻ったらまた 1 回出せる状態に戻す。
func TestAdvisorLoop_OutOfSessionLogsOnceNotEveryMinute(t *testing.T) {
	outOfSession := time.Date(2026, 7, 21, 11, 50, 0, 0, clock.JST) // 昼休み
	adv := &perSymbolAdvisor{}
	var armed []*config.StrategyConfig
	loop := newLoopWithScreeners(t, adv, &armed, outOfSession)

	var lines []string
	loop.Logger = slog.New(capturingHandler{fn: func(msg string) { lines = append(lines, msg) }})

	for i := 0; i < 5; i++ {
		loop.Clock = func() time.Time { return outOfSession.Add(time.Duration(i) * time.Minute) }
		loop.Tick(context.Background())
	}

	n := 0
	for _, m := range lines {
		if m == "advisor_loop_skip_out_of_session" {
			n++
		}
	}
	if n == 0 {
		t.Error("場外に入ったことが 1 度も Info に出ない — 故障で止まっているのと区別が付かない")
	}
	if n > 1 {
		t.Errorf("advisor_loop_skip_out_of_session が %d 回 — 場外の間は 1 回だけでよい", n)
	}
}

// 🛑 **同じ arm を毎ラウンド「armed」と出さない**。
//
// 実測: `advisor_loop_armed` が **341 行**で本日最多。同じ
// (銘柄, 戦略) が最大 33 回出ていた — arm は変わっていないのに、ラウンドごとに
// 再宣言していたため。
//
// 🛑 無音にはしない。**新しく arm されたとき / config_id が変わったときは必ず出す**
// (何を構えたかは監査に要る)。変わっていないラウンドだけ Debug へ落とす。
func TestAdvisorLoop_ArmedLoggedOnlyWhenItChanges(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	adv := &perSymbolAdvisor{}
	var armed []*config.StrategyConfig
	loop := newLoopWithScreeners(t, adv, &armed, inSession)
	loop.Heartbeat = time.Minute

	var lines []string
	loop.Logger = slog.New(capturingHandler{fn: func(msg string) { lines = append(lines, msg) }})

	loop.Tick(context.Background()) // 1 周目: 新規 arm なので出てよい
	firstN := countMsg(lines, "advisor_loop_armed")
	if firstN == 0 {
		t.Fatal("1 周目で armed が 1 度も出ない — 何を構えたかが監査に残らない")
	}

	lines = nil
	for i := 0; i < 3; i++ {
		loop.Clock = func() time.Time { return inSession.Add(time.Duration(i+1) * 2 * time.Minute) }
		loop.Tick(context.Background())
	}
	if n := countMsg(lines, "advisor_loop_armed"); n != 0 {
		t.Errorf("arm が変わっていないのに armed が %d 回 — 実測で同じ銘柄が 33 回出ていた", n)
	}
}

func countMsg(lines []string, want string) int {
	n := 0
	for _, m := range lines {
		if m == want {
			n++
		}
	}
	return n
}
