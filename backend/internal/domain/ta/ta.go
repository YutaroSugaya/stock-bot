// Package ta は純粋なテクニカル指標(I/O なし・状態なし)。データ不足は 0 / ok=false を返す。
package ta

import (
	"math"

	"stockbot/backend/internal/domain/market"
)

func closes(cs []market.Candle) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = c.Close
	}
	return out
}

func SMA(values []float64, n int) float64 {
	if n <= 0 || len(values) < n {
		return 0
	}
	sum := 0.0
	for _, v := range values[len(values)-n:] {
		sum += v
	}
	return sum / float64(n)
}

func SMACloses(cs []market.Candle, n int) float64 { return SMA(closes(cs), n) }

// ATR は前日終値が要るので n+1 本未満は 0。
func ATR(cs []market.Candle, n int) float64 {
	if n <= 0 || len(cs) < n+1 {
		return 0
	}
	trs := make([]float64, 0, n)
	for i := len(cs) - n; i < len(cs); i++ {
		h, l, prevClose := cs[i].High, cs[i].Low, cs[i-1].Close
		tr := math.Max(h-l, math.Max(math.Abs(h-prevClose), math.Abs(l-prevClose)))
		trs = append(trs, tr)
	}
	sum := 0.0
	for _, v := range trs {
		sum += v
	}
	return sum / float64(n)
}

// breakout channel。n 本未満は ok=false。
func Donchian(cs []market.Candle, n int) (high, low float64, ok bool) {
	if n <= 0 || len(cs) < n {
		return 0, 0, false
	}
	high = cs[len(cs)-n].High
	low = cs[len(cs)-n].Low
	for _, c := range cs[len(cs)-n:] {
		if c.High > high {
			high = c.High
		}
		if c.Low < low {
			low = c.Low
		}
	}
	return high, low, true
}

// 直近 n 本の平均ステップ変化(安価なトレンド方向 proxy)。
func Slope(values []float64, n int) float64 {
	if n < 2 || len(values) < n {
		return 0
	}
	w := values[len(values)-n:]
	return (w[len(w)-1] - w[0]) / float64(n-1)
}
