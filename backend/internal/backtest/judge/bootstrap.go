// Package judge is the statistical edge verdict. It is deterministic given a
// seed so a verdict is reproducible.
package judge

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
)

// BootstrapMeanCI is the seeded percentile CI of the mean, resampling one TRADE
// at a time. Errors on empty input.
func BootstrapMeanCI(values []float64, resamples int, confidence float64, seed int64) (lo, hi float64, err error) {
	n := len(values)
	if n == 0 {
		return 0, 0, fmt.Errorf("bootstrap: empty sample")
	}
	if resamples <= 0 {
		resamples = 10000
	}
	if confidence <= 0 || confidence >= 1 {
		confidence = 0.95
	}
	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, resamples)
	for r := 0; r < resamples; r++ {
		sum := 0.0
		for i := 0; i < n; i++ {
			sum += values[rng.Intn(n)]
		}
		means[r] = sum / float64(n)
	}
	sort.Float64s(means)
	alpha := (1 - confidence) / 2
	return quantile(means, alpha), quantile(means, 1-alpha), nil
}

// BootstrapMeanCIDayBlock resamples one CALENDAR DAY at a time instead of one
// trade. forward 標本は多数の銘柄を同時に見ているので、市場全体が動いた日の建玉は
// 互いに独立ではない — トレード単位の復元抽出はその相関を無視し、CI を実際より
// 狭く(楽観側に)出す。
//
// Bonferroni-Šidák の confidence 補正は eval 側の検定族固有なのでここでは行わない。
// days は values と同順同長(JST "2006-01-02")。長さ不一致・空はエラー。
func BootstrapMeanCIDayBlock(values []float64, days []string, resamples int, confidence float64, seed int64) (lo, hi float64, err error) {
	if len(values) == 0 {
		return 0, 0, fmt.Errorf("bootstrap: empty sample")
	}
	if len(days) != len(values) {
		return 0, 0, fmt.Errorf("bootstrap: days/values length mismatch (%d vs %d)", len(days), len(values))
	}
	if resamples <= 0 {
		resamples = 10000
	}
	if confidence <= 0 || confidence >= 1 {
		confidence = 0.95
	}
	// 初出順で日を並べる — map の反復順に依存すると seed 固定でも再現しなくなる。
	order := []string{}
	byDay := map[string][]float64{}
	for i, v := range values {
		if _, ok := byDay[days[i]]; !ok {
			order = append(order, days[i])
		}
		byDay[days[i]] = append(byDay[days[i]], v)
	}
	d := len(order)
	blocks := make([][]float64, d)
	for i, k := range order {
		blocks[i] = byDay[k]
	}

	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, resamples)
	for r := 0; r < resamples; r++ {
		var sum float64
		var cnt int
		// 日ごとの本数が違うので、平均は抽出されたトレード総数で割る
		// (本数の多い日が自然に重く効く)。
		for i := 0; i < d; i++ {
			for _, v := range blocks[rng.Intn(d)] {
				sum += v
				cnt++
			}
		}
		if cnt > 0 {
			means[r] = sum / float64(cnt)
		}
	}
	sort.Float64s(means)
	alpha := (1 - confidence) / 2
	return quantile(means, alpha), quantile(means, 1-alpha), nil
}

func quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return math.NaN()
	}
	if n == 1 {
		return sorted[0]
	}
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// Mean は標本平均。🛑 空は NaN(定義されない)— 0 を返すと「測って 0 だった」と読める
// (JudgeResult.MarshalJSON が NaN を null に出すのと同じ規約)。件数 0 を 0 で出したい
// 呼び手は自分で件数を見る。edge-eval / edge-judge / pair-diff の平均はこれ 1 つ。
func Mean(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}
