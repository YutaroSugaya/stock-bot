package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
	"stockbot/backend/internal/testutil"
	"stockbot/backend/internal/usecase/command"
)

// staleBroker returns a STALE ticker (indicative price present) while delegating
// every other call to the paper engine — modelling a halted / pre-open feed.
type staleBroker struct {
	*broker.Paper
}

func (s staleBroker) GetTicker(ctx context.Context, sym string) (*market.Ticker, error) {
	t, err := s.Paper.GetTicker(ctx, sym)
	if t != nil {
		t.Stale = true
	}
	return t, err
}

func tokyoHours() session.TradingHours {
	return session.TradingHours{
		TZ:          clock.JST,
		Sessions:    []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:30"}},
		EntryCutoff: "14:55",
		ForceFlatAt: "14:50",
	}
}

// The 14:50 引け前フラット化 must fire even when the only available quote is STALE:
// the finding was that PriceTick early-returned on a stale ticker BEFORE reaching
// the flatten, leaving intraday positions to ride into the penalty auto-close.
func TestPriceTick_FlattensIntradayEvenWhenQuoteStale(t *testing.T) {
	ctx := context.Background()
	openAt := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)

	pb := broker.NewPaper(clock.Fixed(openAt), 0, 0)
	pb.SetPrice("7203", 2500)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	closer := repository.NewCloser(posRepo, tradeRepo)
	pending := safety.NewPendingPositions()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "emg.flag"), nil)

	// Open an intraday (一日信用) position through the real entry saga so the paper
	// broker actually holds it.
	exec := command.NewExecuteOrder(pb, posRepo, pending, es, clock.Fixed(openAt))
	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "7203",
		EntryPrice: 2500, TakeProfitJPY: 100, StopLossJPY: 50, Quantity: 100,
		HoldingMode: order.HoldingIntraday,
	}
	if _, err := exec.Execute(ctx, command.ExecuteOrderInput{Signal: sig, Quantity: 100, ExecKind: order.ExecMarginOneday, Source: position.SourceBot}); err != nil {
		t.Fatalf("open intraday position: %v", err)
	}

	// Rewire the time-dependent commands at 14:51 (near close) over a STALE feed.
	nearClose := time.Date(2026, 6, 17, 14, 51, 0, 0, clock.JST)
	c := clock.Fixed(nearClose)
	brk := staleBroker{pb}
	b := &SymbolBundle{
		Symbol:   "7203",
		Broker:   brk,
		Manage:   command.NewManageOpenPositions(brk, posRepo, closer, es, c, 0.30),
		Flatten:  command.NewForceFlatten(brk, posRepo, closer, es, tokyoHours(), c),
		Hours:    tokyoHours(),
		Clock:    c,
		Counters: &Counters{},
	}

	b.PriceTick(ctx)

	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 0 {
		t.Fatalf("intraday position must be force-flattened despite the stale quote; still open: %+v", open)
	}
	if got := b.Counters.ForcedFlats.Load(); got != 1 {
		t.Fatalf("ForcedFlats = %d, want 1", got)
	}
}

// toggleStaleBroker flips a symbol between a tradeable and an INDICATIVE quote
// between ticks (売買停止 → 再開 をモデル化)。
type toggleStaleBroker struct {
	*broker.Paper
	stale bool
}

func (s *toggleStaleBroker) GetTicker(ctx context.Context, sym string) (*market.Ticker, error) {
	t, err := s.Paper.GetTicker(ctx, sym)
	if t != nil {
		t.Stale = s.stale
	}
	return t, err
}

// 終日一度も約定しない銘柄(立花の現在値=0 → 前日終値フォールバック = Stale)は、
// LastSummary が healthy なティックでしか埋まらないため **ダッシュボードから完全に
// 消えていた**。実測: 6976 / 6981 / 285A が建玉を持ったまま現在値も気配も
// 出ず、「売買停止」と「フィード故障」を画面から区別できなかった。
//
// 直し方は「別の口から出す」— indicative を LastSummary に混ぜると前日終値で含み損益が
// 組み上がり、動いていない値段の嘘の損益が画面に出る(この bot が一貫して避けてきた失敗)。
func TestPriceTick_StaleQuoteIsObservableButNeverTradeable(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	c := clock.Fixed(at)

	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("7203", 2500)
	posRepo := repository.NewInMemoryPositionRepo()
	closer := repository.NewCloser(posRepo, repository.NewInMemoryTradeRepo())
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "emg.flag"), nil)
	brk := &toggleStaleBroker{Paper: pb, stale: true}
	b := &SymbolBundle{
		Symbol:   "7203",
		Broker:   brk,
		Agg:      market.NewAggregator("7203", 64),
		Configs:  NewConfigSet(nil),
		Manage:   command.NewManageOpenPositions(brk, posRepo, closer, es, c, 0.30),
		Flatten:  command.NewForceFlatten(brk, posRepo, closer, es, tokyoHours(), c),
		Hours:    tokyoHours(),
		Clock:    c,
		Counters: &Counters{},
	}

	b.PriceTick(ctx)

	if s := b.LastSummary(); s != nil {
		t.Fatalf("indicative な気配が tradeable な summary として出た: %+v", s)
	}
	ind := b.LastIndicative()
	if ind == nil {
		t.Fatal("未約定銘柄の indicative summary が出ていない(画面から銘柄が消える)")
	}
	if ind.CurrentRate.Last != 2500 {
		t.Fatalf("indicative last = %v, want 2500 (前日終値)", ind.CurrentRate.Last)
	}
	if got := b.Counters.StaleQuotes.Load(); got != 1 {
		t.Fatalf("StaleQuotes = %d, want 1", got)
	}

	// 売買再開: tradeable な気配が来たら indicative は下ろす — 「今は気配だけ」の
	// バッジが約定再開後も画面に残り続けてはいけない。
	brk.stale = false
	b.PriceTick(ctx)

	if s := b.LastSummary(); s == nil || s.CurrentRate.Last != 2500 {
		t.Fatalf("再開後の tradeable summary が出ていない: %+v", s)
	}
	if ind := b.LastIndicative(); ind != nil {
		t.Fatalf("約定再開後も indicative が残っている: %+v", ind)
	}
}

// 監視から外れた銘柄(disarm 済み × 建玉なし)は 方式B で poll 自体をやめる。その銘柄の
// 「気配のみ・未約定」バッジを出しっぱなしにすると、**誰も見ていない銘柄について
// 止まっていると言い続ける**ことになる — 条件より長生きするバッジは、バッジが無いより悪い。
// 立てるのと同じ理由で、下ろすのも観測の正しさの一部。
func TestPriceTick_NarrowSkipDropsTheStaleBadge(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	c := clock.Fixed(at)

	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("7203", 2500)
	posRepo := repository.NewInMemoryPositionRepo()
	closer := repository.NewCloser(posRepo, repository.NewInMemoryTradeRepo())
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "emg.flag"), nil)
	brk := &toggleStaleBroker{Paper: pb, stale: true}
	configs := NewConfigSet(nil)
	configs.Arm(&config.StrategyConfig{ConfigID: "armed", StrategyName: config.StrategyBNFReversion})
	b := &SymbolBundle{
		Symbol:        "7203",
		Broker:        brk,
		Agg:           market.NewAggregator("7203", 64),
		Configs:       configs,
		Manage:        command.NewManageOpenPositions(brk, posRepo, closer, es, c, 0.30),
		Flatten:       command.NewForceFlatten(brk, posRepo, closer, es, tokyoHours(), c),
		Hours:         tokyoHours(),
		Clock:         c,
		Counters:      &Counters{},
		MonitorNarrow: true,
		PosRepo:       posRepo,
	}

	b.PriceTick(ctx) // armed なので poll する → 未約定 → バッジが立つ
	if b.LastIndicative() == nil {
		t.Fatal("arm 中の未約定銘柄でバッジが立っていない")
	}

	configs.Disarm(config.StrategyBNFReversion) // disarm。建玉も無いので 方式B が poll をやめる
	b.PriceTick(ctx)

	if ind := b.LastIndicative(); ind != nil {
		t.Fatalf("監視から外れた銘柄のバッジが残った: %+v", ind)
	}
}

// broker 由来の日足は **分割未調整**(立花)。repo が空でシードも無いと、bundle は
// broker から直接引いて戦略に渡す経路があり、そこにガードが無いと 1:5 分割が
// 「見かけ -80% の暴落」として BNF 逆張りの最良シグナルに化ける(レビュー M3)。
func TestRawDailyCandlesRejectsSplitPoisonedBrokerBars(t *testing.T) {
	day := func(d int, c float64) market.Candle {
		return market.Candle{OpenTime: time.Date(2026, 3, d, 0, 0, 0, 0, clock.JST), Open: c, High: c, Low: c, Close: c, Volume: 1}
	}
	b := &SymbolBundle{
		Symbol: "7012",
		Broker: klineOnlyBroker{klines: []market.Candle{day(26, 15000), day(27, 15195), day(30, 2917)}}, // 1:5 分割
	}
	if got := b.rawDailyCandles(context.Background()); got != nil {
		t.Fatalf("分割未調整のバーが戦略に渡った: %+v", got)
	}

	clean := klineOnlyBroker{klines: []market.Candle{day(26, 15000), day(27, 15195), day(30, 15300)}}
	b2 := &SymbolBundle{Symbol: "7012", Broker: clean}
	if got := b2.rawDailyCandles(context.Background()); len(got) != 3 {
		t.Fatalf("正常なバーが捨てられた: %+v", got)
	}
}

// klineOnlyBroker is a paper broker with canned klines (bundle uses GetKlines only here).
type klineOnlyBroker struct {
	*broker.Paper
	klines []market.Candle
}

func (k klineOnlyBroker) GetKlines(context.Context, string, port.KlinePeriod, int) ([]market.Candle, error) {
	return k.klines, nil
}

// countingMarginBroker は維持率照会の回数を数える。
// MOCK rationale (TESTING.md §1 system boundary): 立花の口座照会は wire なので、
// 「何回叩いたか」は数え口を挟む以外に観測できない。
type countingMarginBroker struct {
	*broker.Paper
	margins int
}

func (b *countingMarginBroker) GetAccountMargin(context.Context) (*order.AccountMargin, error) {
	b.margins++
	return &order.AccountMargin{AvailableJPY: 900000, MarginRatio: 1.0, Equity: 900000}, nil
}

// 🛑 維持率は毎ティック broker に訊く質問ではない(規則:
// 保有中は1時間に1回・決済したらその都度・無保有は朝1回)。3秒ティックで場中ずっと
// 訊くと live 1銘柄で 13,200回/日 になり、建玉ゼロの間は同じ定数 1.0 を取り直している。
func TestPriceTick_MarginPollIsThrottled(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	now := at
	c := clock.Clock(func() time.Time { return now })

	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("7203", 2500)
	brk := &countingMarginBroker{Paper: pb}
	posRepo := repository.NewInMemoryPositionRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "emg.flag"), nil)

	b := &SymbolBundle{
		Symbol:             "7203",
		Broker:             brk,
		PosRepo:            posRepo,
		Agg:                market.NewAggregator("7203", 64),
		Manage:             command.NewManageOpenPositions(brk, posRepo, nil, es, c, 0.30),
		Flatten:            command.NewForceFlatten(brk, posRepo, nil, es, tokyoHours(), c),
		Hours:              tokyoHours(),
		Clock:              c,
		Counters:           &Counters{},
		Configs:            NewConfigSet(nil),
		Logger:             testutil.SilentLogger(),
		MarginPollInterval: time.Hour,
	}

	// 起動後の 1 回目は必ず訊く(朝の同期)。
	b.PriceTick(ctx)
	if brk.margins != 1 {
		t.Fatalf("起動直後の照会 = %d回, want 1", brk.margins)
	}
	// 無保有のまま 3 秒ごとに 100 ティック回しても増えない(建玉ゼロ = 維持率は常に 1.0)。
	for i := 0; i < 100; i++ {
		now = now.Add(3 * time.Second)
		b.PriceTick(ctx)
	}
	if brk.margins != 1 {
		t.Fatalf("無保有で %d回 照会した, want 1(建玉ゼロなら訊く理由が無い)", brk.margins)
	}

	// 建玉が入ったら**その場で**取り直す(約定直後の再同期)。
	if _, err := posRepo.AdoptExternal(ctx, port.BrokerPosition{
		BrokerPositionID: "bp-1", Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 2500, ExecKind: order.ExecMarginGeneral,
	}, now); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	now = now.Add(3 * time.Second)
	b.PriceTick(ctx)
	if brk.margins != 2 {
		t.Fatalf("建玉発生時の再同期 = %d回, want 2", brk.margins)
	}

	// 保有中は 1 時間に 1 回。29 分ぶん回しても増えない(昼休みを跨がない範囲で)。
	for i := 0; i < 50; i++ {
		now = now.Add(35 * time.Second)
		b.PriceTick(ctx)
	}
	if brk.margins != 2 {
		t.Fatalf("保有中 29分で %d回, want 2(1時間に1回)", brk.margins)
	}
	now = now.Add(31 * time.Minute)
	b.PriceTick(ctx)
	if brk.margins != 3 {
		t.Fatalf("1時間経過後 = %d回, want 3", brk.margins)
	}
}

// MarginPollInterval 未設定(0)は**現行どおり毎ティック**。paper / backtest / 既存
// テストの挙動を 1bit も変えないための逃げ道。
func TestPriceTick_MarginPollUnthrottledByDefault(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	now := at
	c := clock.Clock(func() time.Time { return now })

	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("7203", 2500)
	brk := &countingMarginBroker{Paper: pb}
	posRepo := repository.NewInMemoryPositionRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "emg.flag"), nil)
	b := &SymbolBundle{
		Symbol: "7203", Broker: brk, PosRepo: posRepo,
		Agg:      market.NewAggregator("7203", 64),
		Manage:   command.NewManageOpenPositions(brk, posRepo, nil, es, c, 0.30),
		Flatten:  command.NewForceFlatten(brk, posRepo, nil, es, tokyoHours(), c),
		Hours:    tokyoHours(),
		Clock:    c,
		Counters: &Counters{},
		Logger:   testutil.SilentLogger(),
		Configs:  NewConfigSet(nil),
	}
	for i := 0; i < 5; i++ {
		now = now.Add(3 * time.Second)
		b.PriceTick(ctx)
	}
	if brk.margins != 5 {
		t.Fatalf("既定は毎ティック: %d回, want 5", brk.margins)
	}
}

// 🚨 **約定 / 決済で口座は変わる。** entry 経路が読むキャッシュはそこで捨てる。
// 🛑 維持率ブレーカーの再照会に相乗りさせない —— ブレーカーは
// `maintenance_ratio<=0` で丸ごと無効にできる knob なので、そこに寄せると
// 「リスク設定を 0 にしたら担保の値が約定後も古いまま」という結び付きが静かに生まれる。
func TestPriceTick_FillInvalidatesTheAccountMarginCache(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	now := at
	c := clock.Clock(func() time.Time { return now })

	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("7203", 2500)
	brk := &countingMarginBroker{Paper: pb}
	posRepo := repository.NewInMemoryPositionRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "emg.flag"), nil)
	cache := command.NewAccountMarginCache(brk, time.Hour, c)

	b := &SymbolBundle{
		Symbol:  "7203",
		Broker:  brk,
		PosRepo: posRepo,
		Agg:     market.NewAggregator("7203", 64),
		// 維持率ブレーカーは無効(0)。それでも無効化は効かなければならない。
		Manage:             command.NewManageOpenPositions(brk, posRepo, nil, es, c, 0),
		Flatten:            command.NewForceFlatten(brk, posRepo, nil, es, tokyoHours(), c),
		Hours:              tokyoHours(),
		Clock:              c,
		Counters:           &Counters{},
		Configs:            NewConfigSet(nil),
		Logger:             testutil.SilentLogger(),
		MarginPollInterval: time.Hour,
		MarginCache:        cache,
	}

	b.PriceTick(ctx) // 起動直後の 1 ティック(建玉なし)
	if _, ok := cache.Get(ctx); !ok {
		t.Fatal("キャッシュを温められない")
	}
	primed := brk.margins
	if _, ok := cache.Get(ctx); !ok || brk.margins != primed {
		t.Fatalf("2 回目の読みで照会が増えた (margins=%d, want %d)", brk.margins, primed)
	}

	if _, err := posRepo.AdoptExternal(ctx, port.BrokerPosition{
		BrokerPositionID: "bp-1", Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 2500, ExecKind: order.ExecMarginGeneral,
	}, now); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	now = now.Add(3 * time.Second)
	b.PriceTick(ctx)

	if _, ok := cache.Get(ctx); !ok {
		t.Fatal("約定後にキャッシュが読めない")
	}
	if brk.margins <= primed {
		t.Fatalf("約定を跨いでも古い値を配り続けた (margins=%d, want > %d)", brk.margins, primed)
	}
}
