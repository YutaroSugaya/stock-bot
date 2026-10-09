package app

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/testutil"
)

func dbar(close, vol float64, day int) market.Candle {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return market.Candle{
		Symbol: "X", Interval: 24 * time.Hour, OpenTime: base.AddDate(0, 0, day),
		Open: close, High: close, Low: close, Close: close, Volume: vol,
	}
}

func calmSeries(n int) []market.Candle {
	cs := make([]market.Candle, n)
	for i := range cs {
		cs[i] = dbar(2000, 1000, i)
	}
	return cs
}

func crashSeries() []market.Candle { // BNF trigger: -15% on ~2x volume
	cs := calmSeries(30)
	cs[29] = dbar(1700, 2000, 29)
	return cs
}

func newSelectorFixture(t *testing.T) (*Selector, map[string]*ActiveConfigHolder, *repository.InMemoryPositionRepo) {
	t.Helper()
	candles := repository.NewInMemoryCandleRepo()
	_ = candles.Upsert(context.Background(), "7203", crashSeries())
	_ = candles.Upsert(context.Background(), "6758", calmSeries(30))
	pos := repository.NewInMemoryPositionRepo()
	holders := map[string]*ActiveConfigHolder{
		"7203": NewActiveConfigHolder(noTradeCfgFor("7203")),
		"6758": NewActiveConfigHolder(noTradeCfgFor("6758")),
	}
	armCfg := func(sym string) *config.StrategyConfig {
		return &config.StrategyConfig{ConfigID: "armed_bnf_" + sym, Symbol: sym, StrategyName: config.StrategyBNFReversion, HoldingMode: config.HoldingMultiday}
	}
	sel := NewSelector(candles, pos, holders, []strategy.Screener{strategy.BNFReversion{}}, 1, 0,
		armCfg, noTradeCfgFor, nil, testutil.SilentLogger())
	return sel, holders, pos
}

func noTradeCfgFor(sym string) *config.StrategyConfig {
	return &config.StrategyConfig{ConfigID: "no_trade_" + sym, Symbol: sym, StrategyName: config.StrategyNoTrade}
}

func TestSelector_ArmsBestCandidateWhenFlat(t *testing.T) {
	sel, holders, _ := newSelectorFixture(t)
	sel.Tick(context.Background())

	if g := holders["7203"].Get(); g == nil || g.StrategyName != config.StrategyBNFReversion {
		t.Fatalf("the BNF crash symbol 7203 should be armed, got %+v", g)
	}
	if g := holders["6758"].Get(); g == nil || g.StrategyName != config.StrategyNoTrade {
		t.Fatalf("the calm symbol 6758 should stay no_trade, got %+v", g)
	}
	if sel.ArmedSymbol() != "7203" {
		t.Fatalf("ArmedSymbol = %q, want 7203", sel.ArmedSymbol())
	}
}

func TestSelector_SlotFullKeepsHeldDoesNotArm(t *testing.T) {
	sel, holders, pos := newSelectorFixture(t)
	// Simulate 6758 already armed AND holding a position (slot full at cap 1).
	holders["6758"].Set(&config.StrategyConfig{ConfigID: "armed_bnf_6758", Symbol: "6758", StrategyName: config.StrategyBNFReversion, HoldingMode: config.HoldingMultiday})
	if _, err := pos.Insert(context.Background(), port.PositionInsertInput{Symbol: "6758", Side: order.SideBuy, Quantity: 100}); err != nil {
		t.Fatalf("seed position: %v", err)
	}

	sel.Tick(context.Background())

	// Held symbol must NOT be disarmed (config freeze).
	if g := holders["6758"].Get(); g == nil || g.StrategyName != config.StrategyBNFReversion {
		t.Fatalf("held 6758 must keep its armed config, got %+v", g)
	}
	// The slot is full → the BNF crash on 7203 must NOT be armed.
	if g := holders["7203"].Get(); g == nil || g.StrategyName != config.StrategyNoTrade {
		t.Fatalf("slot full → 7203 must stay no_trade, got %+v", g)
	}
}

// newSelectorFixtureMulti seeds 3 BNF-crash symbols + 1 calm across a
// configurable account cap, for the multi-position (Phase-2) selection path.
func newSelectorFixtureMulti(t *testing.T, accountMax int) (*Selector, map[string]*ActiveConfigHolder, *repository.InMemoryPositionRepo) {
	t.Helper()
	candles := repository.NewInMemoryCandleRepo()
	for _, sym := range []string{"6758", "7203", "9984"} {
		_ = candles.Upsert(context.Background(), sym, crashSeries())
	}
	_ = candles.Upsert(context.Background(), "4063", calmSeries(30))
	pos := repository.NewInMemoryPositionRepo()
	holders := map[string]*ActiveConfigHolder{
		"6758": NewActiveConfigHolder(noTradeCfgFor("6758")),
		"7203": NewActiveConfigHolder(noTradeCfgFor("7203")),
		"9984": NewActiveConfigHolder(noTradeCfgFor("9984")),
		"4063": NewActiveConfigHolder(noTradeCfgFor("4063")),
	}
	armCfg := func(sym string) *config.StrategyConfig {
		return &config.StrategyConfig{ConfigID: "armed_bnf_" + sym, Symbol: sym, StrategyName: config.StrategyBNFReversion, HoldingMode: config.HoldingMultiday}
	}
	sel := NewSelector(candles, pos, holders, []strategy.Screener{strategy.BNFReversion{}}, accountMax, 0,
		armCfg, noTradeCfgFor, nil, testutil.SilentLogger())
	return sel, holders, pos
}

func armedCount(holders map[string]*ActiveConfigHolder, syms ...string) int {
	n := 0
	for _, sym := range syms {
		if g := holders[sym].Get(); g != nil && g.StrategyName == config.StrategyBNFReversion {
			n++
		}
	}
	return n
}

// With a free account cap of 3 and 3 crash candidates, all 3 arm in one pass
// (the calm symbol stays no_trade). Old ranked[0]-only behaviour armed just 1.
func TestSelector_ArmsMultipleUpToAccountMax(t *testing.T) {
	sel, holders, _ := newSelectorFixtureMulti(t, 3)
	sel.Tick(context.Background())

	if n := armedCount(holders, "6758", "7203", "9984"); n != 3 {
		t.Fatalf("account_max=3 with 3 crash candidates should arm all 3, armed=%d", n)
	}
	if g := holders["4063"].Get(); g == nil || g.StrategyName != config.StrategyNoTrade {
		t.Fatalf("calm 4063 should stay no_trade, got %+v", g)
	}
}

// The top-ranked symbol already holding a position must NOT stall the rest: the
// remaining free slots arm the next-best NOT-held candidates (the bug the
// ranked[0]-only selector had — a held top symbol blocked all further arms).
func TestSelector_FillsRemainingSlotsWhenTopRankedHeld(t *testing.T) {
	sel, holders, pos := newSelectorFixtureMulti(t, 3)
	// 6758 sorts first on the score tiebreak; make it armed AND held.
	holders["6758"].Set(&config.StrategyConfig{ConfigID: "armed_bnf_6758", Symbol: "6758", StrategyName: config.StrategyBNFReversion, HoldingMode: config.HoldingMultiday})
	if _, err := pos.Insert(context.Background(), port.PositionInsertInput{Symbol: "6758", Side: order.SideBuy, Quantity: 100}); err != nil {
		t.Fatalf("seed position: %v", err)
	}

	sel.Tick(context.Background())

	if g := holders["6758"].Get(); g == nil || g.StrategyName != config.StrategyBNFReversion {
		t.Fatalf("held 6758 must keep its armed config (freeze), got %+v", g)
	}
	if n := armedCount(holders, "7203", "9984"); n != 2 {
		t.Fatalf("2 free slots should arm the next-best 2 not-held candidates, armed=%d", n)
	}
}

// account_max unchanged at 1 (the committed default) still arms exactly one.
func TestSelector_AccountMaxOneArmsSingle(t *testing.T) {
	sel, holders, _ := newSelectorFixtureMulti(t, 1)
	sel.Tick(context.Background())

	if n := armedCount(holders, "6758", "7203", "9984"); n != 1 {
		t.Fatalf("account_max=1 must arm exactly one, armed=%d", n)
	}
}

// ArmedSymbols exposes every armed symbol (sorted) for the multi-position
// dashboard; ArmedSymbol is the first of that same set. Both assertions belong
// in one test: `armed_symbol` is the field the UI reads, and its contract is
// exactly "first of ArmedSymbols()", so drifting them apart must fail here.
func TestSelector_ArmedSymbolsReturnsAllArmedSorted(t *testing.T) {
	sel, _, _ := newSelectorFixtureMulti(t, 3)
	sel.Tick(context.Background())

	if got, want := sel.ArmedSymbols(), []string{"6758", "7203", "9984"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ArmedSymbols = %v, want %v", got, want)
	}
	if got := sel.ArmedSymbol(); got != "6758" {
		t.Fatalf("ArmedSymbol = %q, want 6758 (first armed)", got)
	}
}

// crashSeriesAt builds a BNF-trigger crash series priced around `base` (last bar
// -15% on 2x volume), for notional-cap tests where the price level matters.
func crashSeriesAt(base float64) []market.Candle {
	cs := make([]market.Candle, 30)
	for i := 0; i < 29; i++ {
		cs[i] = dbar(base, 1000, i)
	}
	cs[29] = dbar(base*0.85, 2000, 29)
	return cs
}

func notionalFixture(t *testing.T, cap int) (*Selector, map[string]*ActiveConfigHolder) {
	t.Helper()
	candles := repository.NewInMemoryCandleRepo()
	_ = candles.Upsert(context.Background(), "CHEAP", crashSeriesAt(2000))     // round lot: 1700×100 = 170k
	_ = candles.Upsert(context.Background(), "EXPENSIVE", crashSeriesAt(7000)) // round lot: 5950×100 = 595k
	holders := map[string]*ActiveConfigHolder{
		"CHEAP":     NewActiveConfigHolder(noTradeCfgFor("CHEAP")),
		"EXPENSIVE": NewActiveConfigHolder(noTradeCfgFor("EXPENSIVE")),
	}
	armCfg := func(sym string) *config.StrategyConfig {
		c := &config.StrategyConfig{ConfigID: "armed_" + sym, Symbol: sym, StrategyName: config.StrategyBNFReversion, HoldingMode: config.HoldingMultiday}
		c.Risk.Quantity = 100
		return c
	}
	sel := NewSelector(candles, repository.NewInMemoryPositionRepo(), holders,
		[]strategy.Screener{strategy.BNFReversion{}}, 3, cap,
		armCfg, noTradeCfgFor, nil, testutil.SilentLogger())
	return sel, holders
}

// A symbol whose round lot (close × qty) exceeds the notional cap is not armed.
func TestSelector_SkipsSymbolsAboveNotionalCap(t *testing.T) {
	sel, holders := notionalFixture(t, 500_000)
	sel.Tick(context.Background())

	if g := holders["CHEAP"].Get(); g == nil || g.StrategyName != config.StrategyBNFReversion {
		t.Fatalf("CHEAP (lot 170k ≤ 500k) should arm, got %+v", g)
	}
	if g := holders["EXPENSIVE"].Get(); g == nil || g.StrategyName != config.StrategyNoTrade {
		t.Fatalf("EXPENSIVE (lot 595k > 500k) must NOT arm, got %+v", g)
	}
}

// A cap of 0 disables the filter: the expensive symbol arms too.
func TestSelector_NotionalCapZeroDisablesFilter(t *testing.T) {
	sel, holders := notionalFixture(t, 0)
	sel.Tick(context.Background())

	if g := holders["EXPENSIVE"].Get(); g == nil || g.StrategyName != config.StrategyBNFReversion {
		t.Fatalf("cap=0 disables the filter → EXPENSIVE should arm, got %+v", g)
	}
}

// Selector のランキングは単一戦略前提。複数戦略を渡す配線は startup で弾く。
func TestValidateSelectorScreeners_RejectsMultipleStrategies(t *testing.T) {
	err := ValidateSelectorScreeners([]strategy.Screener{
		strategy.BNFReversion{}, strategy.DonchianBreakoutV2{},
	})
	if err == nil {
		t.Fatal("2 戦略を渡したら error (score は戦略間で比較不能 → 上限の無い戦略が全枠を独占する)")
	}
	for _, want := range []string{"bnf_reversion", "donchian_breakout_v2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error は渡された戦略名を挙げるべき: %q に %q が無い", err, want)
		}
	}
}

func TestValidateSelectorScreeners_AllowsSingleAndEmpty(t *testing.T) {
	if err := ValidateSelectorScreeners([]strategy.Screener{strategy.BNFReversion{}}); err != nil {
		t.Fatalf("1 戦略は現行配線そのもの: %v", err)
	}
	if err := ValidateSelectorScreeners(nil); err != nil {
		t.Fatalf("0 戦略は arm 対象なし(strategy.LiveArmable が別途弾く): %v", err)
	}
}

// メニュー全戦略で Name() が取れる(error メッセージが空名になる経路を作らない)。
func TestValidateSelectorScreeners_NamesEveryMenuStrategy(t *testing.T) {
	all := strategy.DefaultScreeners()
	err := ValidateSelectorScreeners(all)
	if err == nil {
		t.Fatalf("DefaultScreeners は %d 戦略なので弾かれるべき", len(all))
	}
	for _, s := range all {
		if n := string(s.Name()); n == "" || !strings.Contains(err.Error(), n) {
			t.Errorf("戦略名が error に出ていない: %q", n)
		}
	}
}

// splitCorruptSeries は「1:5 分割が未調整のまま残った日足」を模す。実勢 400 円
// なのに 25日線が 1680 円に居座るので、乖離 −76% の**偽のパニック**になる。
func splitCorruptSeries() []market.Candle {
	cs := calmSeries(30)
	cs[29] = dbar(400, 3000, 29) // 1:5 の段差。健全な crashSeries より必ず上位に来る
	return cs
}

// 🛑 分割未調整の系列は selector が arm してはいけない。
// bundle 側には段差ガードがあるので**発注はされない**が、selector 側に無いと
// 首位を偽パニックが占め、口座枠(実弾は 1 枠)が塞がったまま何も起きない
// 沈黙のデッドロックになる。8309 が実際にこの状態だった。
func TestSelector_SkipsSplitUnadjustedSeries(t *testing.T) {
	candles := repository.NewInMemoryCandleRepo()
	_ = candles.Upsert(context.Background(), "8309", splitCorruptSeries())
	_ = candles.Upsert(context.Background(), "7203", crashSeries())
	pos := repository.NewInMemoryPositionRepo()
	holders := map[string]*ActiveConfigHolder{
		"8309": NewActiveConfigHolder(noTradeCfgFor("8309")),
		"7203": NewActiveConfigHolder(noTradeCfgFor("7203")),
	}
	armCfg := func(sym string) *config.StrategyConfig {
		return &config.StrategyConfig{ConfigID: "armed_bnf_" + sym, Symbol: sym, StrategyName: config.StrategyBNFReversion, HoldingMode: config.HoldingMultiday}
	}
	sel := NewSelector(candles, pos, holders, []strategy.Screener{strategy.BNFReversion{}}, 1, 0,
		armCfg, noTradeCfgFor, nil, testutil.SilentLogger())

	sel.Tick(context.Background())

	if g := holders["8309"].Get(); g == nil || g.StrategyName != config.StrategyNoTrade {
		t.Fatalf("分割未調整の 8309 を arm してはいけない, got %+v", g)
	}
	if g := holders["7203"].Get(); g == nil || g.StrategyName != config.StrategyBNFReversion {
		t.Fatalf("枠は健全な 7203 に回るべき, got %+v", g)
	}
}

// --- 「スキャン対象 200」だけでは live の実像を伝えない。1単元が資金上限を超える銘柄は
// arm されない = 買われないので、UI はその内数を出す。ここはその内数の契約。 ---

// 最後の Tick 時点で「1単元が上限内」だった銘柄数を返す。
func TestSelector_AffordableCountReportsUsableUniverse(t *testing.T) {
	sel, _ := notionalFixture(t, 500_000)
	sel.Tick(context.Background())

	if got := sel.AffordableCount(); got != 1 {
		t.Fatalf("AffordableCount = %d, want 1 (CHEAP 170k のみ・EXPENSIVE 595k は上限超)", got)
	}
	if got := sel.MaxNotionalJPY(); got != 500_000 {
		t.Fatalf("MaxNotionalJPY = %d, want 500000", got)
	}
}

// cap 0(フィルタ無効)なら全銘柄が建玉可。
func TestSelector_AffordableCountCapZeroCountsWholeUniverse(t *testing.T) {
	sel, _ := notionalFixture(t, 0)
	sel.Tick(context.Background())

	if got := sel.AffordableCount(); got != 2 {
		t.Fatalf("AffordableCount = %d, want 2 (cap 0 はフィルタ無効)", got)
	}
}

// 🛑 枠が埋まっていても数える。ここが本題 — 「200銘柄を見ている」と読める画面のまま
// 実際には半分しか買えない、という取り違えは**枠が埋まっている時ほど**起きる
// (空き枠が無いと arm も走らないので、画面から実像を確かめる手段が他に無い)。
func TestSelector_AffordableCountComputedEvenWhenSlotsFull(t *testing.T) {
	ctx := context.Background()
	candles := repository.NewInMemoryCandleRepo()
	_ = candles.Upsert(ctx, "CHEAP", crashSeriesAt(2000))
	_ = candles.Upsert(ctx, "EXPENSIVE", crashSeriesAt(7000))
	pos := repository.NewInMemoryPositionRepo()
	holders := map[string]*ActiveConfigHolder{
		"CHEAP":     NewActiveConfigHolder(noTradeCfgFor("CHEAP")),
		"EXPENSIVE": NewActiveConfigHolder(noTradeCfgFor("EXPENSIVE")),
	}
	armCfg := func(sym string) *config.StrategyConfig {
		c := &config.StrategyConfig{ConfigID: "armed_" + sym, Symbol: sym, StrategyName: config.StrategyBNFReversion, HoldingMode: config.HoldingMultiday}
		c.Risk.Quantity = 100
		return c
	}
	// accountMax 1 に対して 1 本建玉 → 空き枠ゼロ。
	if _, err := pos.Insert(ctx, port.PositionInsertInput{Symbol: "CHEAP", Side: order.SideBuy, Quantity: 100}); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	sel := NewSelector(candles, pos, holders, []strategy.Screener{strategy.BNFReversion{}}, 1, 500_000,
		armCfg, noTradeCfgFor, nil, testutil.SilentLogger())

	sel.Tick(ctx)

	if got := sel.AffordableCount(); got != 1 {
		t.Fatalf("AffordableCount = %d, want 1 — 枠が満杯でも建玉可の内数は数える", got)
	}
}

// Tick 前は「まだ数えていない」。0 を返すと「全銘柄が上限超」と区別できない。
func TestSelector_AffordableCountUnknownBeforeFirstTick(t *testing.T) {
	sel, _ := notionalFixture(t, 500_000)

	if got := sel.AffordableCount(); got != -1 {
		t.Fatalf("AffordableCount = %d, want -1 (未計測)", got)
	}
}

// 資金上限を超える銘柄は arm されないので、画面の「発火中の候補」に数えると
// **絶対に発注されないものを発注直前のように見せる**ことになる。どの銘柄が上限外かを
// 内数と同じ Tick から出す(6728 が発火 → 787,000 で上限外 → 未 arm)。
func TestSelector_UnaffordableSymbolsNamesTheOnesAboveCap(t *testing.T) {
	sel, _ := notionalFixture(t, 500_000)
	sel.Tick(context.Background())

	// 🛑 キーは **(銘柄, 戦略)**(優先ティア導入)。ティアごとに建玉金額
	// 上限が違う(live: bnf 550,000 / donchian_v2_trail 300,000)ので、銘柄だけのキーだと
	// 「donchian の上限で落ちた」ことが bnf の行にも「上限超」の印を付けてしまう。
	got := sel.UnaffordableSymbols()
	want := UnaffordableKey("EXPENSIVE", config.StrategyBNFReversion)
	if len(got) != 1 || !got[want] {
		t.Fatalf("UnaffordableSymbols = %v, want {%s:true}", got, want)
	}
}

// cap 0(フィルタ無効)なら上限外は存在しない。
func TestSelector_UnaffordableSymbolsEmptyWhenCapDisabled(t *testing.T) {
	sel, _ := notionalFixture(t, 0)
	sel.Tick(context.Background())

	if got := sel.UnaffordableSymbols(); len(got) != 0 {
		t.Fatalf("UnaffordableSymbols = %v, want empty (cap 0)", got)
	}
}

// Tick 前は「まだ分からない」= 空。画面側は印を付けないだけで、誤って
// 「上限内」と断定した表示にはならない。
func TestSelector_UnaffordableSymbolsEmptyBeforeFirstTick(t *testing.T) {
	sel, _ := notionalFixture(t, 500_000)

	if got := sel.UnaffordableSymbols(); len(got) != 0 {
		t.Fatalf("UnaffordableSymbols = %v, want empty (未計測)", got)
	}
}

// 🚨 事前登録は「250 を 2 箇所に散らしたのが原因だから、**数字を上げるだけでなく 1 箇所に
// 寄せろ**」と書いた。Evaluate 側の供給(`bundle.go`)は `strategy.DailyBarsRequired` へ
// 寄せたが、**Screen 側の供給(`selectorCandleLookback`)は裸の 300 が残っていた** —
// しかもコメントの根拠は「100日移動平均」で、実際の要求(`high_52w_momentum.Screen` =
// 253本)より小さい値だった。52週高値が 4 週間 建玉ゼロだった事故と
// 同じ形。数字ではなく**紐付け**を縛る。
func TestSelectorLookbackIsDerivedFromTheStrategyRequirement(t *testing.T) {
	if selectorCandleLookback <= strategy.DailyBarsRequired {
		t.Fatalf("スクリーニングへの供給 %d 本が戦略の要求 %d 本を満たさない — "+
			"要求の大きいアームは screen 段階で毎回落ち、枠が回らない(= 標本ゼロ)",
			selectorCandleLookback, strategy.DailyBarsRequired)
	}
	// 定数を書き換えても追随すること(裸の数字に戻ったら落ちる)。
	if selectorCandleLookback != strategy.DailyBarsRequired+selectorLookbackHeadroom {
		t.Fatalf("selectorCandleLookback が DailyBarsRequired に紐づいていない(%d)— "+
			"要求本数の大きい戦略を足したときに直し忘れる", selectorCandleLookback)
	}
}
