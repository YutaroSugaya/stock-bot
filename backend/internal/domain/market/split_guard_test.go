package market

import (
	"math"
	"stockbot/backend/internal/domain/clock"
	"testing"
	"time"
)

func jstDay(y int, m time.Month, d int, close float64) Candle {
	return Candle{OpenTime: time.Date(y, m, d, 0, 0, 0, 0, clock.JST),
		Open: close, High: close, Low: close, Close: close, Volume: 1}
}

// broker 由来の日足は分割未調整。取り込む前に検出して捨てる(1:5 分割 = 見かけ -80%)。
func TestSplitDiscontinuityDetectsSplit(t *testing.T) {
	bars := []Candle{jstDay(2026, 3, 27, 15195), jstDay(2026, 3, 30, 2917)} // 1:5 分割
	if d := SplitDiscontinuity(bars); d == "" {
		t.Fatal("1:5 分割を検出できていない")
	}
}

// 値幅制限いっぱいの実相場(-25〜-30%)は分割ではない。弾くと BNF が狙う暴落銘柄の更新が恒久的に止まる。
func TestSplitDiscontinuityAllowsLimitDownMove(t *testing.T) {
	for _, ratio := range []float64{0.72, 0.75, 1.33} { // ±25〜33% の実相場
		bars := []Candle{jstDay(2026, 3, 27, 1000), jstDay(2026, 3, 30, 1000*ratio)}
		if d := SplitDiscontinuity(bars); d != "" {
			t.Fatalf("実相場の値動き(x%.2f)を分割と誤検知: %s", ratio, d)
		}
	}
}

// 最小の分割比(1:1.5 = 併合含む)は検出できること。
func TestSplitDiscontinuityDetectsSmallestSplitRatio(t *testing.T) {
	bars := []Candle{jstDay(2026, 3, 27, 1000), jstDay(2026, 3, 30, 1500)}
	if d := SplitDiscontinuity(bars); d == "" {
		t.Fatal("1:1.5 の分割/併合を検出できていない")
	}
}

// 日付キーは JST に正規化する(pg は TIMESTAMPTZ を UTC で返し得る。正規化しないと同日チェックが黙って無効化)。
func TestSameDayLevelMismatch(t *testing.T) {
	utc := Candle{OpenTime: time.Date(2026, 3, 30, 6, 0, 0, 0, time.UTC), Close: 1000} // = JST 15:00
	fetched := Candle{OpenTime: time.Date(2026, 3, 30, 0, 0, 0, 0, clock.JST), Close: 5000}
	if d := SameDayLevelMismatch([]Candle{utc}, []Candle{fetched}); d == "" {
		t.Fatal("TZ をまたぐ同日比較で 5倍の水準差を検出できていない(調整の有無が違う)")
	}
	same := Candle{OpenTime: fetched.OpenTime, Close: 1010}
	if d := SameDayLevelMismatch([]Candle{utc}, []Candle{same}); d != "" {
		t.Fatalf("同水準を誤検知: %s", d)
	}
}

// 空・1本は判定対象なし。
func TestSplitDiscontinuityShortInput(t *testing.T) {
	if d := SplitDiscontinuity(nil); d != "" {
		t.Fatalf("nil で検出: %s", d)
	}
	if d := SplitDiscontinuity([]Candle{jstDay(2026, 3, 30, 100)}); d != "" {
		t.Fatalf("1本で検出: %s", d)
	}
}

// 同日水準チェックは継ぎ目付近だけを見る。窓を広げると chain-link 済み履歴を持つ銘柄が恒久的に
// 更新できなくなる(実際に 18 銘柄がロックされた)。
func TestSameDayLevelMismatchOnlyChecksRecentOverlap(t *testing.T) {
	stored := []Candle{
		jstDay(2026, 1, 5, 500), // 分割前の水準(chain-link 済みなので古い日は調整後)
		jstDay(2026, 6, 15, 990), jstDay(2026, 6, 16, 995), jstDay(2026, 6, 17, 1000),
		jstDay(2026, 6, 18, 1005), jstDay(2026, 6, 19, 1000), // ここから直近5本
		jstDay(2026, 6, 22, 1010),
	}
	fetched := []Candle{
		jstDay(2026, 1, 5, 2500),  // 取得側は未調整(分割前の生値)= ここは比較しない
		jstDay(2026, 6, 22, 1012), // 継ぎ目付近は一致している
		jstDay(2026, 6, 23, 1020),
	}
	if d := SameDayLevelMismatch(RecentWindow(stored, 5), fetched); d != "" {
		t.Fatalf("継ぎ目付近は一致しているのに拒否した: %s", d)
	}
	// 継ぎ目付近が食い違うケースは当然拒否する。
	bad := []Candle{jstDay(2026, 6, 22, 5050), jstDay(2026, 6, 23, 5100)}
	if d := SameDayLevelMismatch(RecentWindow(stored, 5), bad); d == "" {
		t.Fatal("継ぎ目付近の水準ズレを見逃した")
	}
}

func TestRecentWindow(t *testing.T) {
	bars := []Candle{jstDay(2026, 6, 1, 1), jstDay(2026, 6, 2, 2), jstDay(2026, 6, 3, 3)}
	if got := RecentWindow(bars, 2); len(got) != 2 || got[0].Close != 2 {
		t.Fatalf("RecentWindow = %+v", got)
	}
	if got := RecentWindow(bars, 10); len(got) != 3 {
		t.Fatalf("要求が多いときは全部返す: %+v", got)
	}
	if got := RecentWindow(nil, 3); got != nil {
		t.Fatalf("nil = %+v", got)
	}
}

// 単一ソースでは分割は拒否ではなく自動 chain-link で吸収する。単純分割比に一致しない急変は絶対に均さない。
func TestSplitRatio(t *testing.T) {
	cases := []struct {
		prev, cur float64
		want      float64 // 0 = 分割ではない
	}{
		{15195, 2917, 0.2}, // 1:5 分割(株価は 1/5)
		{1000, 5000, 5},    // 1:5 併合相当
		{1000, 1500, 1.5},  // 最小比
		{1000, 700, 0},     // -30% の実相場 → 調整しない
		{1000, 720, 0},     // -28%
		{3120, 2420, 0},    // 6594 の実例(-22.4%)
		{1000, 1010, 0},    // 通常
	}
	for _, c := range cases {
		got, ok := SplitRatio(c.prev, c.cur)
		if c.want == 0 {
			if ok {
				t.Fatalf("%.0f→%.0f を分割と誤判定(ratio=%v)", c.prev, c.cur, got)
			}
			continue
		}
		if !ok {
			t.Fatalf("%.0f→%.0f の分割を検出できず", c.prev, c.cur)
		}
		if math.Abs(got/c.want-1) > 0.01 {
			t.Fatalf("%.0f→%.0f: ratio = %v, want ≈%v", c.prev, c.cur, got, c.want)
		}
	}
}

// ChainLinkSplits: 分割以前のバーを比で調整して連続化する(出来高は逆比)。
func TestChainLinkSplits(t *testing.T) {
	bars := []Candle{
		{OpenTime: t0(1), Open: 15000, High: 15200, Low: 14900, Close: 15195, Volume: 100},
		{OpenTime: t0(2), Open: 2900, High: 2950, Low: 2890, Close: 2917, Volume: 500}, // 1:5 分割
		{OpenTime: t0(3), Open: 2920, High: 2960, Low: 2900, Close: 2950, Volume: 480},
	}
	out, n := ChainLinkSplits(bars)
	if n != 1 {
		t.Fatalf("調整件数 = %d, want 1", n)
	}
	if d := SplitDiscontinuity(out); d != "" {
		t.Fatalf("調整後も断裂が残る: %s", d)
	}
	if math.Abs(out[0].Close-3039) > 1 { // 15195 / 5
		t.Fatalf("古い側の終値 = %v, want ≈3039", out[0].Close)
	}
	if math.Abs(out[0].Volume-500) > 1 { // 100 * 5
		t.Fatalf("出来高 = %v, want ≈500(逆比)", out[0].Volume)
	}
	// 分割が無ければ何もしない。
	if _, n := ChainLinkSplits(out); n != 0 {
		t.Fatalf("冪等でない: %d", n)
	}
}

func t0(d int) time.Time {
	return time.Date(2026, 3, d, 0, 0, 0, 0, clock.JST)
}

// 実データ 8001(2025-12-29)は 1:5 分割当日に +5% 動く。深い比だけ許容を広げる(浅い比は実暴落と重なるので広げない)。
func TestSplitRatioDeepRatioTolerance(t *testing.T) {
	if _, ok := SplitRatio(9565, 2015); !ok { // ratio 0.2107 = 1:5 分割 + 当日 +5%
		t.Fatal("実データの 1:5 分割(当日 +5%)を検出できていない")
	}
	// 深い比を緩めても、実相場の下落は依然として分割扱いしない。
	for _, c := range [][2]float64{{1000, 700}, {3120, 2420}, {1000, 650}} {
		if r, ok := SplitRatio(c[0], c[1]); ok {
			t.Fatalf("%.0f→%.0f を分割と誤判定(ratio=%v)", c[0], c[1], r)
		}
	}
}

// 🛑 1:6 / 1:7 の分割が候補に無く**検出できていなかった**。
//
//	5803  27,630 → 4,505  = 1/6.13
//	7013  17,495 → 2,623  = 1/6.67
//
// どちらも CSV 段階で未調整のまま残り、逆張り(BNF)から見て -84% の理想的な
// パニックに化けていた。深い下落比を候補に足しても**実暴落と混同しない**:
// 日本株には値幅制限があり、1日で -83% は制度上あり得ない。
func TestSplitRatio_DetectsDeepSplits(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur float64
		wantRatio float64
	}{
		{"5803 1:6", 27630, 4505, 1.0 / 6},
		{"7013 1:7", 17495, 2623, 1.0 / 7},
		{"8309 1:4", 6842, 1672, 1.0 / 4},
		{"1:8", 8000, 1000, 1.0 / 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := SplitRatio(c.prev, c.cur)
			if !ok {
				t.Fatalf("分割として検出されなかった(%v → %v)", c.prev, c.cur)
			}
			if math.Abs(got/c.wantRatio-1) > 0.01 {
				t.Errorf("ratio=%v, want ≈%v", got, c.wantRatio)
			}
		})
	}
}

// 🛑 候補を広げても**本物の暴落は分割にしない**。BNF はパニックを取りに行く戦略
// なので、実際の急落を分割として均してしまうとエントリー機会そのものが消える。
//
// 判別が成立する根拠は**値幅制限**: 日本株の 1 日の値動きには上下限があり、
// ¥1,000 の銘柄なら概ね ±¥300(30%)。それを超える段差は制度上ありえないので
// コーポレートアクション(分割 / 併合)と読んでよい。この前提が崩れる市場に
// 展開するときは候補リストごと見直すこと。
func TestSplitRatio_KeepsRealCrashes(t *testing.T) {
	for _, c := range []struct {
		name      string
		prev, cur float64
	}{
		{"-20%", 1000, 800},
		{"-30%(値幅制限いっぱい)", 1000, 700},
		{"+25%", 1000, 1250},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := SplitRatio(c.prev, c.cur); ok {
				t.Errorf("実際の値動きを分割として扱った(%v → %v)", c.prev, c.cur)
			}
		})
	}
}
