package ta

import (
	"math"
	"testing"

	"stockbot/backend/internal/domain/market"
)

func mkCandles(highs, lows, cl []float64) []market.Candle {
	out := make([]market.Candle, len(cl))
	for i := range cl {
		out[i] = market.Candle{High: highs[i], Low: lows[i], Close: cl[i]}
	}
	return out
}

func TestSMA(t *testing.T) {
	if got := SMA([]float64{1, 2, 3, 4}, 2); got != 3.5 {
		t.Fatalf("SMA last2 = %g, want 3.5", got)
	}
	if got := SMA([]float64{1, 2}, 5); got != 0 {
		t.Fatalf("SMA too-short = %g, want 0", got)
	}
}

func TestATR(t *testing.T) {
	cs := mkCandles(
		[]float64{10, 11, 12, 13},
		[]float64{9, 10, 11, 12},
		[]float64{9.5, 10.5, 11.5, 12.5},
	)
	// TR for last 3 candles each ~= 1 (high-low) vs prior close.
	got := ATR(cs, 3)
	if got <= 0 {
		t.Fatalf("ATR = %g, want >0", got)
	}
}

func TestDonchian(t *testing.T) {
	cs := mkCandles(
		[]float64{10, 15, 12},
		[]float64{8, 9, 7},
		[]float64{9, 14, 11},
	)
	h, l, ok := Donchian(cs, 3)
	if !ok || h != 15 || l != 7 {
		t.Fatalf("Donchian = (%g,%g,%v), want (15,7,true)", h, l, ok)
	}
	if _, _, ok := Donchian(cs, 5); ok {
		t.Fatal("Donchian with too few candles should be ok=false")
	}
}

func TestSlope(t *testing.T) {
	if got := Slope([]float64{1, 2, 3, 4, 5}, 5); math.Abs(got-1) > 1e-9 {
		t.Fatalf("Slope = %g, want 1", got)
	}
	if got := Slope([]float64{5, 4, 3}, 3); math.Abs(got-(-1)) > 1e-9 {
		t.Fatalf("Slope down = %g, want -1", got)
	}
}
