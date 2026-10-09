package position

import (
	"math"
	"testing"
)

// ¥1M 正規化(EDGE_METHODOLOGY の共通規約)は forward-report / edge-eval / pair-diff が
// 同じ 1 つを通る。建玉金額が出せない行は 0(呼び手が落とすか数えるかを決める)。
func TestPer1MNotional(t *testing.T) {
	cases := []struct {
		name       string
		jpy, entry float64
		qty        int
		want       float64
	}{
		{"建値 2500 × 100 株で 1000 円 = 4000/1M", 1000, 2500, 100, 4000},
		{"建値 1000 × 100 株で 50 円 = 500/1M", 50, 1000, 100, 500},
		{"建値 0 は出せない", 1000, 0, 100, 0},
		{"株数 0 は出せない", 1000, 2500, 0, 0},
		{"負の株数は金額として正で扱う(符号を反転させない)", 1000, 2500, -100, 4000},
		{"NaN の建値は出せない", 1000, math.NaN(), 100, 0},
	}
	for _, c := range cases {
		if got := Per1MNotional(c.jpy, c.entry, c.qty); got != c.want {
			t.Errorf("%s: Per1MNotional(%v, %v, %d) = %v, want %v", c.name, c.jpy, c.entry, c.qty, got, c.want)
		}
	}
}
