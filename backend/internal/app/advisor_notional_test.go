package app

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// advisor_notional_test.go は資金キャパシティ・フィルタを固定する。
// **実弾で買えない値がさ株で戦略を検証しても意味がない**
// ので、最小単元(100株)の建玉金額が上限を超える銘柄はユニバースごと外す。
// paper 口座は1億のままだが、対象銘柄だけを「実際に張れる価格帯」に絞る。

// priceSeries は panicSeries と同じ BNF パニック形を、任意の株価水準で作る
// (RankCandidates が拾う形は保ったまま、建玉金額だけを変える)。
func priceSeries(sym string, base float64) []market.Candle {
	cs := make([]market.Candle, 0, 40)
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 39; i++ {
		cs = append(cs, market.Candle{Symbol: sym, OpenTime: t0.AddDate(0, 0, i),
			Open: base, High: base, Low: base, Close: base, Volume: 1000})
	}
	last := base * 0.85
	cs = append(cs, market.Candle{Symbol: sym, OpenTime: t0.AddDate(0, 0, 39),
		Open: last, High: last, Low: last, Close: last, Volume: 3000})
	return cs
}

func notionalLoop(t *testing.T, cap int, syms map[string]float64) *AdvisorLoop {
	t.Helper()
	repo := repository.NewInMemoryCandleRepo()
	names := make([]string, 0, len(syms))
	for sym, base := range syms {
		if err := repo.Upsert(context.Background(), sym, priceSeries(sym, base)); err != nil {
			t.Fatal(err)
		}
		names = append(names, sym)
	}
	return &AdvisorLoop{
		Symbols:                names,
		Candles:                repo,
		MaxPositionNotionalJPY: cap,
		Hours:                  tokyoTradingHours(),
	}
}

// 100株の建玉金額が上限を超える銘柄は universe から落ちる = ランキングにも
// LLM ラウンドにも一切現れない(トークンも使わない)。
func TestAdvisorUniverseDropsUnaffordableSymbols(t *testing.T) {
	// 直近終値 = base*0.85。7203: 8,500円 → 100株 85万(可)/
	// 285A: 40,800円 → 100株 408万(不可)
	loop := notionalLoop(t, 1_000_000, map[string]float64{"7203": 10000, "285A": 48000})
	u := loop.universe(context.Background())
	if _, ok := u["7203"]; !ok {
		t.Fatalf("100株 85万の銘柄は残すこと: %v", keysOf(u))
	}
	if _, ok := u["285A"]; ok {
		t.Fatalf("100株 408万の銘柄は上限 100万で外すこと: %v", keysOf(u))
	}
}

// 上限ちょうどは含む(<=)。境界で銘柄が消える方向に倒すと、閾値の意味が
// 「未満」にすり替わって台帳の記述と食い違う。
func TestAdvisorUniverseIncludesExactlyAtCap(t *testing.T) {
	// 直近終値 10,000円 → 100株 = ちょうど 100万
	loop := notionalLoop(t, 1_000_000, map[string]float64{"7203": 10000 / 0.85})
	if _, ok := loop.universe(context.Background())["7203"]; !ok {
		t.Fatal("ちょうど上限の銘柄は含めること(<=)")
	}
}

// cap=0 は無効(従来どおり全銘柄)。研究モードの既定を壊さない後方互換。
func TestAdvisorUniverseCapZeroDisabled(t *testing.T) {
	loop := notionalLoop(t, 0, map[string]float64{"7203": 10000, "285A": 48000})
	if got := len(loop.universe(context.Background())); got != 2 {
		t.Fatalf("cap=0 は無効(全銘柄)のはず: %d 銘柄", got)
	}
}

// LLM が単元より大きい株数を返した場合、銘柄が価格帯を満たしていても
// **建玉金額が上限を超えるなら arm しない**。ユニバース側のフィルタは
// 「最小単元で買えるか」しか見ていないので、ここが第二の関門になる。
func TestAdvisorRejectsArmWhenQuantityBlowsTheCap(t *testing.T) {
	loop := notionalLoop(t, 1_000_000, map[string]float64{"7203": 10000})
	daily := priceSeries("7203", 10000) // 直近終値 8,500円

	ok100 := &config.StrategyConfig{Symbol: "7203", StrategyName: config.StrategyBNFReversion}
	ok100.Risk.Quantity = 100 // 85万 → 可
	if err := loop.withinNotionalCap(ok100, daily); err != nil {
		t.Fatalf("85万は通すこと: %v", err)
	}

	tooBig := &config.StrategyConfig{Symbol: "7203", StrategyName: config.StrategyBNFReversion}
	tooBig.Risk.Quantity = 300 // 255万 → 不可
	if err := loop.withinNotionalCap(tooBig, daily); err == nil {
		t.Fatal("255万は上限 100万で弾くこと(単元より大きい株数の抜け道を塞ぐ)")
	}

	// no_trade は建玉しないので金額の検査対象外(黙って落とすと
	// 「なぜ見送ったか」の記録が消える)。
	nt := &config.StrategyConfig{Symbol: "7203", StrategyName: config.StrategyNoTrade}
	if err := loop.withinNotionalCap(nt, daily); err != nil {
		t.Fatalf("no_trade は対象外: %v", err)
	}
}

// 価格が取れないときは fail-close(通さない)。金額を検証できないまま
// 建てる経路を残さない。
func TestAdvisorNotionalCapFailsClosedWithoutPrice(t *testing.T) {
	loop := notionalLoop(t, 1_000_000, map[string]float64{"7203": 10000})
	c := &config.StrategyConfig{Symbol: "7203", StrategyName: config.StrategyBNFReversion}
	c.Risk.Quantity = 100
	if err := loop.withinNotionalCap(c, nil); err == nil {
		t.Fatal("価格不明では通さないこと(fail-close)")
	}
}

func keysOf(m map[string][]market.Candle) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ port.CandleRepository = repository.NewInMemoryCandleRepo()

// 終値 0(売買停止・気配のみ)は「価格不明」として fail-close する。0 を素通り
// させると `0*100 <= cap` が true になり、**価格を検証できないまま**銘柄が残る
// (レビュー指摘: コメントは fail-close と書いてあるのに fail-open だった)。
func TestAdvisorNotionalTreatsZeroCloseAsUnknownPrice(t *testing.T) {
	loop := notionalLoop(t, 1_000_000, map[string]float64{"7203": 10000})
	halted := priceSeries("7203", 10000)
	for i := range halted {
		halted[i].Close = 0
	}
	if loop.affordableLot(halted) {
		t.Fatal("終値 0 の銘柄をユニバースに残してはいけない(fail-close)")
	}
	c := &config.StrategyConfig{Symbol: "7203", StrategyName: config.StrategyBNFReversion}
	c.Risk.Quantity = 100
	if err := loop.withinNotionalCap(c, halted); err == nil {
		t.Fatal("終値 0 では建玉金額を検証できないので arm しないこと")
	}
}
