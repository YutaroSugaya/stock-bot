package app

import (
	"context"
	"testing"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/testutil"
)

// 優先ティア —— **前の段が枠を取り切ったら次の段は動かない**
// (「bnf の発火を最優先し、枠に余裕があるときに限り donchian_v2_trail を発火させる」)。
//
// 🚨 なぜラウンドロビンでも単純な合成ランキングでもないか:
// `ValidateSelectorScreeners` が禁じているとおり、score は戦略間で比較不能
// (abs_momentum は 現在値/200日線 で上限なし、donchian は 現在値/20日高値 で 1.0 付近が上限)。
// 降順に混ぜると上限の無い戦略が全枠を独占する(実測: 80枠中 73 が abs_momentum、
// 19銘柄トリガーした donchian は 0 件)。**優先ティアは score を一度も跨いで比較しない**
// ので、この問題を構造的に回避する — ランキングは常に 1 ティア = 1 戦略の中でだけ起きる。

// tierScreener は指定した銘柄だけを triggered にして返すスタブ。
type tierScreener struct {
	name    config.StrategyName
	scores  map[string]float64 // symbol -> score(載っていない銘柄は未トリガー)
	stopJPY float64
}

func (s tierScreener) Name() config.StrategyName { return s.name }

func (s tierScreener) Screen(symbol string, d []market.Candle) strategy.Candidate {
	sc, ok := s.scores[symbol]
	return strategy.Candidate{
		Symbol: symbol, Strategy: s.name, Triggered: ok, Score: sc, StopLossJPY: s.stopJPY,
	}
}

// 全銘柄 終値 2,000 円 × 100株 = 建玉 200,000。上限の効きを見たい試験は個別に価格を変える。
func tieredFixture(t *testing.T, accountMax int, symbols []string, tiers []SelectorTier) (*Selector, map[string]*ActiveConfigHolder) {
	t.Helper()
	candles := repository.NewInMemoryCandleRepo()
	holders := map[string]*ActiveConfigHolder{}
	for _, sym := range symbols {
		if err := candles.Upsert(context.Background(), sym, calmSeries(30)); err != nil {
			t.Fatalf("upsert %s: %v", sym, err)
		}
		holders[sym] = NewActiveConfigHolder(noTradeCfgFor(sym))
	}
	sel := NewSelectorTiered(candles, repository.NewInMemoryPositionRepo(), holders, tiers,
		accountMax, noTradeCfgFor, nil, testutil.SilentLogger())
	return sel, holders
}

func armCfgFor(name config.StrategyName) func(string) *config.StrategyConfig {
	return func(sym string) *config.StrategyConfig {
		c := &config.StrategyConfig{ConfigID: string(name) + "_" + sym, Symbol: sym,
			StrategyName: name, HoldingMode: config.HoldingMultiday}
		c.Risk.Quantity = 100
		return c
	}
}

func bnfThenDonchian(bnfHits, donHits map[string]float64) []SelectorTier {
	return []SelectorTier{
		{
			Screeners: []strategy.Screener{tierScreener{name: config.StrategyBNFReversion, scores: bnfHits}},
			ArmCfg:    armCfgFor(config.StrategyBNFReversion),
		},
		{
			Screeners: []strategy.Screener{tierScreener{name: config.StrategyDonchianBreakoutV2Trail, scores: donHits}},
			ArmCfg:    armCfgFor(config.StrategyDonchianBreakoutV2Trail),
		},
	}
}

func armedName(h *ActiveConfigHolder) config.StrategyName {
	if c := h.Get(); c != nil {
		return c.StrategyName
	}
	return config.StrategyNoTrade
}

// bnf が枠を取り切ったら donchian は 1 本も arm されない。
func TestSelectorTiers_FirstTierConsumesAllSlots(t *testing.T) {
	sel, holders := tieredFixture(t, 2, []string{"A", "B", "C"},
		bnfThenDonchian(map[string]float64{"A": 2.0, "B": 1.5}, map[string]float64{"C": 3.0}))
	sel.Tick(context.Background())

	for _, sym := range []string{"A", "B"} {
		if got := armedName(holders[sym]); got != config.StrategyBNFReversion {
			t.Fatalf("%s は bnf が arm すべき: %v", sym, got)
		}
	}
	if got := armedName(holders["C"]); got != config.StrategyNoTrade {
		t.Fatalf("枠が無いのに donchian を arm した: %v", got)
	}
}

// bnf が余らせたら donchian が残り枠を埋める(優先順位であって排他ではない)。
func TestSelectorTiers_SecondTierFillsRemainingSlots(t *testing.T) {
	sel, holders := tieredFixture(t, 3, []string{"A", "C", "D"},
		bnfThenDonchian(map[string]float64{"A": 2.0}, map[string]float64{"C": 3.0, "D": 2.5}))
	sel.Tick(context.Background())

	if got := armedName(holders["A"]); got != config.StrategyBNFReversion {
		t.Fatalf("A は bnf: %v", got)
	}
	for _, sym := range []string{"C", "D"} {
		if got := armedName(holders[sym]); got != config.StrategyDonchianBreakoutV2Trail {
			t.Fatalf("%s は余り枠を donchian が埋めるべき: %v", sym, got)
		}
	}
}

// 🛑 同じ銘柄が両ティアで triggered なら **上位ティアが勝つ**。
// live のナンピン禁止キーは (銘柄, 側) なので、1 銘柄に 2 戦略は載せられない。
func TestSelectorTiers_HigherTierWinsTheSameSymbol(t *testing.T) {
	sel, holders := tieredFixture(t, 2, []string{"A"},
		bnfThenDonchian(map[string]float64{"A": 1.0}, map[string]float64{"A": 9.9}))
	sel.Tick(context.Background())

	if got := armedName(holders["A"]); got != config.StrategyBNFReversion {
		t.Fatalf("下位ティアが同一銘柄を奪った: %v", got)
	}
}

// ティアごとに別の建玉金額上限が効く(bnf 550,000 / donchian 300,000)。
func TestSelectorTiers_PerTierNotionalCap(t *testing.T) {
	candles := repository.NewInMemoryCandleRepo()
	// 終値 4,000 × 100株 = 400,000。bnf(550,000)には収まり donchian(300,000)には収まらない。
	mid := make([]market.Candle, 30)
	for i := range mid {
		mid[i] = dbar(4000, 1000, i)
	}
	if err := candles.Upsert(context.Background(), "MID", mid); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	holders := map[string]*ActiveConfigHolder{"MID": NewActiveConfigHolder(noTradeCfgFor("MID"))}

	tiers := bnfThenDonchian(map[string]float64{}, map[string]float64{"MID": 3.0})
	tiers[0].MaxNotionalJPY = 550000
	tiers[1].MaxNotionalJPY = 300000

	sel := NewSelectorTiered(candles, repository.NewInMemoryPositionRepo(), holders, tiers,
		4, noTradeCfgFor, nil, testutil.SilentLogger())
	sel.Tick(context.Background())

	if got := armedName(holders["MID"]); got != config.StrategyNoTrade {
		t.Fatalf("donchian の上限 300,000 を超える 400,000 を arm した: %v", got)
	}

	// 同じ銘柄を bnf 側(上限 550,000)のティアに置けば arm される = 上限がティア別に効いている証拠。
	tiers2 := bnfThenDonchian(map[string]float64{"MID": 3.0}, map[string]float64{})
	tiers2[0].MaxNotionalJPY = 550000
	tiers2[1].MaxNotionalJPY = 300000
	holders2 := map[string]*ActiveConfigHolder{"MID": NewActiveConfigHolder(noTradeCfgFor("MID"))}
	sel2 := NewSelectorTiered(candles, repository.NewInMemoryPositionRepo(), holders2, tiers2,
		4, noTradeCfgFor, nil, testutil.SilentLogger())
	sel2.Tick(context.Background())

	if got := armedName(holders2["MID"]); got != config.StrategyBNFReversion {
		t.Fatalf("bnf の上限 550,000 に収まる 400,000 を arm しなかった: %v", got)
	}
}

// 上限超で落ちた銘柄は **(銘柄, 戦略) キー**で報告する。ティアごとに上限が違うので、
// 銘柄だけのキーだと「bnf では収まるのに donchian の上限で落ちた」行が
// bnf の行にも「上限超」の印を付けてしまう。
func TestSelectorTiers_UnaffordableIsKeyedBySymbolAndStrategy(t *testing.T) {
	candles := repository.NewInMemoryCandleRepo()
	mid := make([]market.Candle, 30)
	for i := range mid {
		mid[i] = dbar(4000, 1000, i) // 400,000
	}
	if err := candles.Upsert(context.Background(), "MID", mid); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	holders := map[string]*ActiveConfigHolder{"MID": NewActiveConfigHolder(noTradeCfgFor("MID"))}

	tiers := bnfThenDonchian(map[string]float64{"MID": 1.0}, map[string]float64{"MID": 1.0})
	tiers[0].MaxNotionalJPY = 550000
	tiers[1].MaxNotionalJPY = 300000

	sel := NewSelectorTiered(candles, repository.NewInMemoryPositionRepo(), holders, tiers,
		4, noTradeCfgFor, nil, testutil.SilentLogger())
	sel.Tick(context.Background())

	tooBig := sel.UnaffordableSymbols()
	if tooBig[UnaffordableKey("MID", config.StrategyBNFReversion)] {
		t.Fatal("bnf(上限 550,000)で 400,000 を上限超と報告した")
	}
	if !tooBig[UnaffordableKey("MID", config.StrategyDonchianBreakoutV2Trail)] {
		t.Fatal("donchian(上限 300,000)で 400,000 を上限超と報告していない")
	}
}
