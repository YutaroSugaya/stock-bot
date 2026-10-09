package market_test

import (
	"math"
	"testing"

	"stockbot/backend/internal/domain/market"
)

// 表は一次資料相当(独立2ソースが全 34 帯で一致)を**リテラルで固定**する。
// 「だいたいこのくらい」で書かないための杭 — 値が変わったらテストが落ちる。
func TestPriceLimitWidth_Table(t *testing.T) {
	cases := []struct {
		base  float64
		width float64
	}{
		{1, 30}, {99.9, 30},
		{100, 50}, {199, 50},
		{200, 80}, {499, 80},
		{500, 100}, {699, 100},
		{700, 150}, {999, 150},
		{1_000, 300}, {1_499, 300},
		{1_500, 400}, {1_999, 400},
		{2_000, 500}, {2_999, 500},
		{3_000, 700}, {4_999, 700},
		{5_000, 1_000}, {6_999, 1_000},
		{7_000, 1_500}, {9_999, 1_500},
		{10_000, 3_000}, {14_999, 3_000},
		{15_000, 4_000},
		{20_000, 5_000},
		{30_000, 7_000},
		{50_000, 10_000},
		{70_000, 15_000},
		{100_000, 30_000},
		{150_000, 40_000},
		{200_000, 50_000},
		{300_000, 70_000},
		{500_000, 100_000},
		{700_000, 150_000},
		{1_000_000, 300_000},
		{1_500_000, 400_000},
		{2_000_000, 500_000},
		{3_000_000, 700_000},
		{5_000_000, 1_000_000},
		{7_000_000, 1_500_000},
		{10_000_000, 3_000_000},
		{15_000_000, 4_000_000},
		{20_000_000, 5_000_000},
		{30_000_000, 7_000_000},
		{50_000_000, 10_000_000}, {99_000_000, 10_000_000},
	}
	for _, c := range cases {
		if got := market.PriceLimitWidth(c.base); got != c.width {
			t.Errorf("PriceLimitWidth(%v) = %v, want %v", c.base, got, c.width)
		}
	}
}

// 🛑 実測との突き合わせ: 前日終値 6,851 の銘柄がストップ安 5,851 で張り付いた日の板
// (bid=ask=5851・last 無し)。表がこの 1 件と合わないなら表が間違っている。
func TestPriceLimit_Matches4704Observation(t *testing.T) {
	const prevClose = 6851.0
	down, ok := market.LimitDown(prevClose)
	if !ok || down != 5851 {
		t.Fatalf("LimitDown(%v) = %v (ok=%v), want 5851 — ストップ安の実測と食い違う", prevClose, down, ok)
	}
	if up, ok := market.LimitUp(prevClose); !ok || up != 7851 {
		t.Fatalf("LimitUp(%v) = %v, want 7851", prevClose, up)
	}
}

// 基準値段が無い(0 / 負)ときは 0 を返す。**呼び手が「制限なし」と読まないこと**が
// 前提の契約なので、ここで勝手に広い値を返さない。
func TestPriceLimitWidth_NoBase(t *testing.T) {
	if got := market.PriceLimitWidth(0); got != 0 {
		t.Fatalf("PriceLimitWidth(0) = %v, want 0", got)
	}
	if _, ok := market.LimitUp(0); ok {
		t.Fatal("基準値段が無ければ ok=false")
	}
}

// 低位株では制限値幅が基準値段を上回る。**0 円は東証に存在しない値段**なので
// 値として返さず「求められない」を返す(呼び手が fail-close できるように)。
func TestLimitDown_BelowZeroIsUnknown(t *testing.T) {
	if got, ok := market.LimitDown(20); ok { // 幅 30 > 基準 20
		t.Fatalf("LimitDown(20) = %v (ok=%v) — 0 円を値段として返さない", got, ok)
	}
	if got, ok := market.LimitDown(30); ok { // ちょうど 0 になる境界
		t.Fatalf("LimitDown(30) = %v (ok=%v) — 0 円ちょうども値段ではない", got, ok)
	}
	if got, ok := market.LimitDown(31); !ok || got != 1 {
		t.Fatalf("LimitDown(31) = %v (ok=%v), want 1", got, ok)
	}
}

// 🚨 NaN / ±Inf は `base <= 0` をすり抜けて**最終行(最大幅)**に落ちていた =
// 異常な入力に最も楽観的な答えを返す fail-open。表の外は「不明」に倒す。
func TestPriceLimitWidth_RejectsNonFinite(t *testing.T) {
	for _, base := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -100} {
		if got := market.PriceLimitWidth(base); got != 0 {
			t.Errorf("PriceLimitWidth(%v) = %v, want 0", base, got)
		}
		if _, ok := market.LimitUp(base); ok {
			t.Errorf("LimitUp(%v) が ok=true を返した", base)
		}
		if _, ok := market.LimitDown(base); ok {
			t.Errorf("LimitDown(%v) が ok=true を返した", base)
		}
	}
}

// 最終行のセンチネル(upto=0 = 上限なし)を守る杭。これを消すと最高価格帯が
// 「基準値段なし」に化ける(到達不能だった `return 0` に落ちる)。
func TestPriceLimitWidth_TopBandSentinel(t *testing.T) {
	if got := market.PriceLimitWidth(1e12); got != 10_000_000 {
		t.Fatalf("PriceLimitWidth(1e12) = %v, want 10000000", got)
	}
}

// 幅は**基準値段だけ**で決まり、結果価格では決まらない(一番間違えやすい性質)。
func TestLimitUp_WidthFromBaseNotResult(t *testing.T) {
	if got, ok := market.LimitUp(999); !ok || got != 1149 {
		t.Fatalf("LimitUp(999) = %v, want 1149(結果は 1,000 超だが幅は 150 のまま)", got)
	}
}

// 🛑 実測 2 件目(帯が別): 基準 460 → ストップ高 540。
func TestPriceLimit_Matches6904Observation(t *testing.T) {
	if got, ok := market.LimitUp(460); !ok || got != 540 {
		t.Fatalf("LimitUp(460) = %v (ok=%v), want 540 — ストップ高の実測と食い違う", got, ok)
	}
}

// 帯の**上端直下**が隣へこぼれないこと(表の杭は下端だけでは半分)。
func TestPriceLimitWidth_UpperEdges(t *testing.T) {
	cases := []struct{ base, width float64 }{
		{19_999, 4_000}, {29_999, 5_000}, {49_999, 7_000}, {69_999, 10_000},
		{99_999, 15_000}, {149_999, 30_000}, {199_999, 40_000}, {299_999, 50_000},
		{499_999, 70_000}, {699_999, 100_000}, {999_999, 150_000}, {1_499_999, 300_000},
		{1_999_999, 400_000}, {2_999_999, 500_000}, {4_999_999, 700_000}, {6_999_999, 1_000_000},
		{9_999_999, 1_500_000}, {14_999_999, 3_000_000}, {19_999_999, 4_000_000},
		{29_999_999, 5_000_000}, {49_999_999, 7_000_000},
	}
	for _, c := range cases {
		if got := market.PriceLimitWidth(c.base); got != c.width {
			t.Errorf("PriceLimitWidth(%v) = %v, want %v", c.base, got, c.width)
		}
	}
}
