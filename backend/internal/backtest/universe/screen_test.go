package universe

import (
	"math"
	"testing"
	"time"

	"stockbot/backend/internal/domain/market"
)

func bars(n int, close, vol float64) []market.Candle {
	cs := make([]market.Candle, n)
	for i := range cs {
		cs[i] = market.Candle{Open: close, High: close, Low: close, Close: close, Volume: vol}
	}
	return cs
}

// datedBars builds n daily candles ending on lastDay (YYYY-MM-DD, 1本/日)。
func datedBars(n int, lastDay string, close, vol float64) []market.Candle {
	end, err := time.Parse("2006-01-02", lastDay)
	if err != nil {
		panic(err)
	}
	cs := bars(n, close, vol)
	for i := range cs {
		cs[i].OpenTime = end.AddDate(0, 0, -(n - 1 - i))
	}
	return cs
}

func byName(rs []Result) map[string]Result {
	m := make(map[string]Result, len(rs))
	for _, r := range rs {
		m[r.Symbol] = r
	}
	return m
}

func TestScreenAppliesBothCriteria(t *testing.T) {
	u := map[string][]market.Candle{
		// 終値 2,000円 × 出来高 500万 = 売買代金 100億(通過)/ 単元 20万(通過)
		"7203": bars(60, 2000, 5_000_000),
		// 終値 40,000円 × 出来高 20万 = 80億(通過)/ 単元 400万(高すぎ)
		"285A": bars(60, 40000, 200_000),
		// 終値 1,000円 × 出来高 10万 = 1億(薄い)/ 単元 10万(安い)
		"9999": bars(60, 1000, 100_000),
	}
	rs := Screen(u, Criteria{MedianTurnoverJPY: 5_000_000_000, MaxLotNotionalJPY: 1_000_000})
	got := byName(rs)

	if !got["7203"].Passed {
		t.Fatalf("流動 かつ 単元が安い銘柄は通すこと: %+v", got["7203"])
	}
	if got["285A"].Passed || got["285A"].Reason != "lot_too_expensive" {
		t.Fatalf("単元 400万は落とすこと: %+v", got["285A"])
	}
	if got["9999"].Passed || got["9999"].Reason != "illiquid" {
		t.Fatalf("売買代金 1億は落とすこと: %+v", got["9999"])
	}
	if p := Passed(rs); len(p) != 1 || p[0] != "7203" {
		t.Fatalf("Passed = %v, want [7203]", p)
	}
}

func TestScreenReportsIlliquidBeforeExpensive(t *testing.T) {
	u := map[string][]market.Candle{"X": bars(60, 40000, 1000)} // 薄い かつ 高い
	got := byName(Screen(u, Criteria{MedianTurnoverJPY: 5_000_000_000, MaxLotNotionalJPY: 1_000_000}))
	if got["X"].Reason != "illiquid" {
		t.Fatalf("Reason = %q, want illiquid(両方該当なら流動性を優先して記録)", got["X"].Reason)
	}
}

func TestScreenUsesMedianNotMean(t *testing.T) {
	cs := bars(60, 1000, 100_000) // 通常は売買代金 1億
	cs[59].Volume = 1_000_000_000 // 1日だけ異常出来高(売買代金 1兆)
	got := byName(Screen(map[string][]market.Candle{"X": cs},
		Criteria{MedianTurnoverJPY: 5_000_000_000}))
	if got["X"].Passed {
		t.Fatalf("1日の異常出来高で通してはいけない(中央値で判定): %+v", got["X"])
	}
}

func TestScreenCountsHaltedDaysAsZeroTurnover(t *testing.T) {
	cs := bars(60, 1000, 20_000_000) // 売買代金 200億
	for i := 0; i < 31; i++ {        // 過半数を売買停止に
		cs[i].Volume = 0
	}
	got := byName(Screen(map[string][]market.Candle{"X": cs},
		Criteria{MedianTurnoverJPY: 5_000_000_000}))
	if got["X"].Passed {
		t.Fatalf("過半数が売買停止の銘柄を通してはいけない: median=%v", got["X"].MedianTurnoverJPY)
	}
}

func TestScreenRejectsInsufficientHistory(t *testing.T) {
	got := byName(Screen(map[string][]market.Candle{"X": bars(10, 1000, 20_000_000)},
		Criteria{MedianTurnoverJPY: 1, Lookback: 60}))
	if got["X"].Passed || got["X"].Reason != "insufficient_history" {
		t.Fatalf("履歴 10 本では判定しないこと: %+v", got["X"])
	}
}

func TestScreenZeroThresholdDisablesCriterion(t *testing.T) {
	u := map[string][]market.Candle{"285A": bars(60, 40000, 200_000)}
	got := byName(Screen(u, Criteria{MedianTurnoverJPY: 5_000_000_000})) // 金額条件なし
	if !got["285A"].Passed {
		t.Fatalf("MaxLotNotionalJPY=0 は金額条件なしのはず: %+v", got["285A"])
	}
}

func TestScreenTopNKeepsOnlyTheMostLiquid(t *testing.T) {
	u := map[string][]market.Candle{
		"1111": bars(60, 1000, 30_000_000), // 300億
		"2222": bars(60, 1000, 20_000_000), // 200億
		"3333": bars(60, 1000, 10_000_000), // 100億
	}
	got := byName(Screen(u, Criteria{MedianTurnoverJPY: 5_000_000_000, TopN: 2}))
	if !got["1111"].Passed || !got["2222"].Passed {
		t.Fatalf("上位2銘柄は通すこと: %+v %+v", got["1111"], got["2222"])
	}
	if got["3333"].Passed || got["3333"].Reason != "below_top_n" {
		t.Fatalf("3位は below_top_n で落とすこと: %+v", got["3333"])
	}
	if got["1111"].Rank != 1 || got["2222"].Rank != 2 || got["3333"].Rank != 3 {
		t.Fatalf("順位を記録すること(落ちた銘柄にも): %d %d %d",
			got["1111"].Rank, got["2222"].Rank, got["3333"].Rank)
	}
}

func TestScreenTopNRanksByLiquidityNotLotNotional(t *testing.T) {
	u := map[string][]market.Candle{
		"HIGHPRICE": bars(60, 9000, 1_000_000),  // 単元90万・売買代金 90億
		"LOWPRICE":  bars(60, 100, 200_000_000), // 単元1万・売買代金 200億
	}
	got := byName(Screen(u, Criteria{MedianTurnoverJPY: 5_000_000_000, MaxLotNotionalJPY: 1_000_000, TopN: 1}))
	if !got["LOWPRICE"].Passed || got["HIGHPRICE"].Passed {
		t.Fatalf("売買代金の大きい方を残すこと(単元金額順ではない): %+v %+v", got["LOWPRICE"], got["HIGHPRICE"])
	}
}

// 毎朝走らせるので、同値でも同じ入力からは必ず同じユニバースが出ること。
func TestScreenTopNTieBreakIsDeterministic(t *testing.T) {
	u := map[string][]market.Candle{}
	for _, s := range []string{"1111", "2222", "3333", "4444", "5555", "6666"} {
		u[s] = bars(60, 1000, 20_000_000) // 全銘柄が同じ売買代金
	}
	for i := 0; i < 20; i++ {
		got := byName(Screen(u, Criteria{MedianTurnoverJPY: 1, TopN: 3}))
		if !got["1111"].Passed || !got["2222"].Passed || !got["3333"].Passed {
			t.Fatalf("同値なら銘柄コード昇順で埋めること: %+v", got)
		}
		if got["4444"].Passed || got["5555"].Passed || got["6666"].Passed {
			t.Fatalf("同値でも上位3件だけ: %+v", got)
		}
	}
}

func TestScreenTopNLargerThanPopulation(t *testing.T) {
	u := map[string][]market.Candle{"1111": bars(60, 1000, 20_000_000), "2222": bars(60, 1000, 20_000_000)}
	if p := Passed(Screen(u, Criteria{MedianTurnoverJPY: 1, TopN: 500})); len(p) != 2 {
		t.Fatalf("TopN > 母数 なら全件通過: %v", p)
	}
}

func TestScreenTopNDoesNotResurrectThresholdFailures(t *testing.T) {
	u := map[string][]market.Candle{
		"1111": bars(60, 1000, 20_000_000), // 200億(通過)
		"2222": bars(60, 1000, 100_000),    // 1億(薄い)
		"3333": bars(60, 40000, 1_000_000), // 単元400万(高い)
		"4444": bars(60, 1000, 10_000_000), // 100億(通過)
	}
	got := byName(Screen(u, Criteria{MedianTurnoverJPY: 5_000_000_000, MaxLotNotionalJPY: 1_000_000, TopN: 3}))
	if got["2222"].Passed || got["2222"].Reason != "illiquid" {
		t.Fatalf("閾値落ちは TopN の枠に関わらず illiquid のまま: %+v", got["2222"])
	}
	if got["3333"].Passed || got["3333"].Reason != "lot_too_expensive" {
		t.Fatalf("閾値落ちは TopN の枠に関わらず lot_too_expensive のまま: %+v", got["3333"])
	}
	if got["2222"].Rank != 0 || got["3333"].Rank != 0 {
		t.Fatalf("閾値落ちは順位を持たない(枠を消費しない): %d %d", got["2222"].Rank, got["3333"].Rank)
	}
	if !got["1111"].Passed || !got["4444"].Passed {
		t.Fatalf("閾値通過が2件なら TopN=3 では両方残る: %+v %+v", got["1111"], got["4444"])
	}
}

func TestScreenTopNZeroIsUnlimited(t *testing.T) {
	u := map[string][]market.Candle{"1111": bars(60, 1000, 30_000_000), "2222": bars(60, 1000, 20_000_000)}
	got := byName(Screen(u, Criteria{MedianTurnoverJPY: 1}))
	if !got["1111"].Passed || !got["2222"].Passed {
		t.Fatalf("TopN=0 は無制限: %+v", got)
	}
	if got["1111"].Rank != 1 || got["2222"].Rank != 2 {
		t.Fatalf("TopN=0 でも順位は記録する: %d %d", got["1111"].Rank, got["2222"].Rank)
	}
}

// 末尾60本を見るだけで「その末尾がいつのバーか」を見ないと、売買停止で更新が
// 止まった銘柄が停止前の売買代金で上位に残り続ける(実際に踏んだ穴)。

func TestScreenDropsStaleSymbols(t *testing.T) {
	fresh := datedBars(60, "2026-08-06", 1000, 20_000_000)
	halted := datedBars(60, "2026-06-30", 1000, 30_000_000) // 停止前は最も流動的だった
	got := byName(Screen(map[string][]market.Candle{"FRESH": fresh, "HALT": halted},
		Criteria{MedianTurnoverJPY: 1, MaxStaleDays: 7}))
	if got["HALT"].Passed || got["HALT"].Reason != "stale" {
		t.Fatalf("最終バーが古い銘柄は stale で落とすこと: %+v", got["HALT"])
	}
	if !got["FRESH"].Passed {
		t.Fatalf("最新の銘柄は通すこと: %+v", got["FRESH"])
	}
	// 停止銘柄は順位も持たない(枠を消費しない)。
	if got["HALT"].Rank != 0 {
		t.Fatalf("stale は順位を持たない: %d", got["HALT"].Rank)
	}
}

// 基準はデータセット内の最新バーであって暦日ではないので、連休では落とさない。
func TestScreenStaleToleratesWeekendGap(t *testing.T) {
	got := byName(Screen(map[string][]market.Candle{
		"A": datedBars(60, "2026-08-06", 1000, 20_000_000),
		"B": datedBars(60, "2026-08-03", 1000, 20_000_000), // 3日前まで
	}, Criteria{MedianTurnoverJPY: 1, MaxStaleDays: 7}))
	if !got["B"].Passed {
		t.Fatalf("3日の遅れは許容する(連休): %+v", got["B"])
	}
}

func TestScreenStaleDisabledByZero(t *testing.T) {
	got := byName(Screen(map[string][]market.Candle{
		"A": datedBars(60, "2026-08-06", 1000, 20_000_000),
		"B": datedBars(60, "2025-01-06", 1000, 20_000_000),
	}, Criteria{MedianTurnoverJPY: 1}))
	if !got["B"].Passed {
		t.Fatalf("MaxStaleDays=0 は鮮度を課さない: %+v", got["B"])
	}
}

// CSV ローダは ParseFloat のエラーを捨てるので "NaN"/"inf" が値として通る。NaN だと
// 全ての閾値比較が false になり、**無条件で通過して1位を取る**。
func TestScreenRejectsNaNAndInf(t *testing.T) {
	nan := bars(60, 1000, 20_000_000)
	nan[59].Close = math.NaN()
	inf := bars(60, 1000, 20_000_000)
	for i := range inf {
		inf[i].Volume = math.Inf(1)
	}
	got := byName(Screen(map[string][]market.Candle{"NAN": nan, "INF": inf, "OK": bars(60, 1000, 20_000_000)},
		Criteria{MedianTurnoverJPY: 5_000_000_000, MaxLotNotionalJPY: 1_000_000, TopN: 3}))
	if got["NAN"].Passed || got["NAN"].Reason != "no_price" {
		t.Fatalf("NaN 終値は no_price で落とすこと: %+v", got["NAN"])
	}
	if got["INF"].Passed || got["INF"].Reason != "no_price" {
		t.Fatalf("Inf 売買代金は no_price で落とすこと: %+v", got["INF"])
	}
	if !got["OK"].Passed {
		t.Fatalf("正常な銘柄は通ること: %+v", got["OK"])
	}
}

// タイブレークは比較関数の中で完結し、`Screen` が先に銘柄コード順へソートしている
// **前提に依存しない** — 依存すると呼び出し順を変えただけで、テストが緑のまま
// 選定結果が変わる。
func TestRankByLiquidityIsSelfContained(t *testing.T) {
	out := []Result{ // わざと銘柄コード順に並んでいない入力
		{Symbol: "9999", MedianTurnoverJPY: 100, Passed: true},
		{Symbol: "1111", MedianTurnoverJPY: 100, Passed: true},
		{Symbol: "5555", MedianTurnoverJPY: 100, Passed: true},
	}
	rankByLiquidity(out, 2)
	got := byName(out)
	if !got["1111"].Passed || !got["5555"].Passed || got["9999"].Passed {
		t.Fatalf("同値なら銘柄コード昇順(入力順に依存しない): %+v", out)
	}
}

// map の反復順がユニバースを変えてはいけない。
func TestScreenIsDeterministic(t *testing.T) {
	u := map[string][]market.Candle{
		"9999": bars(60, 1000, 20_000_000),
		"1111": bars(60, 1000, 20_000_000),
		"5555": bars(60, 1000, 20_000_000),
	}
	first := Screen(u, Criteria{MedianTurnoverJPY: 1})
	for i := 0; i < 20; i++ {
		got := Screen(u, Criteria{MedianTurnoverJPY: 1})
		for j := range got {
			if got[j].Symbol != first[j].Symbol {
				t.Fatalf("反復ごとに順序が変わる: %v vs %v", got[j].Symbol, first[j].Symbol)
			}
		}
	}
	if first[0].Symbol != "1111" || first[2].Symbol != "9999" {
		t.Fatalf("銘柄コード昇順でない: %+v", first)
	}
}
