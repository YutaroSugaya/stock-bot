package market

import (
	"math"
	"testing"
)

// 2026-09-29 の 6368(オルガノ・1:5 分割の権利落ち日): 前日終値 12,595 → 寄り 2,495。
// 前日終値を基準にした値幅制限(12,595 ± 3,000)の外 = 実相場では約定し得ない値段。
func TestExDateSplitRatio_DetectsOrganoOneToFive(t *testing.T) {
	r, ok := ExDateSplitRatio(12595, 2495)
	if !ok {
		t.Fatal("1:5 分割の権利落ち日を検出できていない")
	}
	if math.Abs(r-5) > 1e-9 {
		t.Fatalf("株数の倍率 = %v, want 5", r)
	}
}

// 1:1.5 は SplitRatio(日足の chain-link)では下落方向を分割扱いしない(実暴落と区別できない)。
// ここでは**値幅制限の外**であることが実暴落でない証明になるので、拾ってよい。
func TestExDateSplitRatio_ShallowSplitOutsideBand(t *testing.T) {
	// 1,200 円 → 800 円: ストップ安は 900 円なので 800 円は実相場ではあり得ない。
	r, ok := ExDateSplitRatio(1200, 800)
	if !ok || math.Abs(r-1.5) > 1e-9 {
		t.Fatalf("1:1.5 分割 = (%v, %v), want (1.5, true)", r, ok)
	}
}

// 🛑 値幅制限の内側の動きは、比が単純分割比に似ていても**絶対に**分割と読まない。
// 読むと本物の暴落で SL を割り引き、守りが効かなくなる(fail-open)。
// 100 円帯は幅 50 円 = ちょうど半値まで動ける。半値は帯の**上**(内側)なので分割ではない。
func TestExDateSplitRatio_InsideBandIsNeverASplit(t *testing.T) {
	cases := []struct{ prev, price float64 }{
		{100, 50},     // ストップ安ちょうど(1:2 と同じ比)
		{12595, 9600}, // 帯の内側の急落
		{1000, 700},   // -30%
		{2000, 2600},  // 急騰
	}
	for _, c := range cases {
		if r, ok := ExDateSplitRatio(c.prev, c.price); ok {
			t.Fatalf("帯の内側の値動き %v → %v を分割(x%v)と誤読", c.prev, c.price, r)
		}
	}
}

// 帯の外でも単純分割比に一致しなければ分割と断定しない(データ異常・特別気配の誤配信など)。
func TestExDateSplitRatio_OutsideBandButNotSimpleRatio(t *testing.T) {
	if r, ok := ExDateSplitRatio(10000, 5800); ok { // x0.58: 1/2 から 16%・1/1.5 から 13%(浅い比の許容は 5%)
		t.Fatalf("単純分割比でない乖離を分割(x%v)と読んだ", r)
	}
}

// 併合(10:1)は株数が減る = 倍率 < 1。
func TestExDateSplitRatio_ReverseSplit(t *testing.T) {
	r, ok := ExDateSplitRatio(300, 3000)
	if !ok || math.Abs(r-0.1) > 1e-9 {
		t.Fatalf("10:1 併合 = (%v, %v), want (0.1, true)", r, ok)
	}
}

func TestExDateSplitRatio_InvalidInputs(t *testing.T) {
	for _, c := range [][2]float64{{0, 100}, {100, 0}, {-1, 5}, {math.NaN(), 1}} {
		if _, ok := ExDateSplitRatio(c[0], c[1]); ok {
			t.Fatalf("不正な入力 %v を分割と読んだ", c)
		}
	}
}

// 🚨 2026-09-29 の 8766(東京海上・1:15 分割の権利落ち日): 前日 8,075 → 523.2。比の表に 15 が無く、
// 日足の chain-link も建玉の言い直しも「単純分割比でない段差」として素通りしていた。
func TestSplitRatios_TokioMarineOneToFifteen(t *testing.T) {
	if r, ok := ExDateSplitRatio(8075, 523.2); !ok || math.Abs(r-15) > 1e-9 {
		t.Fatalf("ExDateSplitRatio(8075, 523.2) = (%v, %v), want (15, true)", r, ok)
	}
	if r, ok := SplitRatio(8075, 523.2); !ok || math.Abs(r-1.0/15) > 1e-9 {
		t.Fatalf("SplitRatio(8075, 523.2) = (%v, %v), want (1/15, true)", r, ok)
	}
	// 実在する他の深い比も(隣の比と取り違えない)。
	for _, n := range []float64{12, 25, 30, 40, 50} {
		if r, ok := SplitRatio(1000, 1000/n); !ok || math.Abs(r-1/n) > 1e-9 {
			t.Errorf("1:%v 分割 = (%v, %v)", n, r, ok)
		}
	}
}
