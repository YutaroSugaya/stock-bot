package market

import (
	"strconv"
	"testing"
	"time"
)

func TestTickSize(t *testing.T) {
	cases := []struct {
		price float64
		want  float64
	}{
		{500, 1}, {3000, 1}, {3001, 5}, {5000, 5}, {5001, 10},
		{30000, 10}, {50000, 50}, {300000, 100}, {5_000_000, 5000}, {6_000_000, 10000},
	}
	for _, tc := range cases {
		if got := TickSize(tc.price); got != tc.want {
			t.Fatalf("TickSize(%g) = %g, want %g", tc.price, got, tc.want)
		}
	}
}

// 粗いテーブルの帯ごとの丸め。symbol は細刻み表に**無い**ものを使う(表に無い銘柄は
// 粗いテーブルへ倒れる = TickSizeOf == TickSize)。tick_fine_test は細刻み側を見るので、
// 粗い側の帯(特に tick 10 帯と tick 5 の上下丸め)を拘束するのはここだけ。
func TestRoundToTickOfCoarseBands(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{2503.4, 2503}, // tick 1
		{4502, 4500},   // tick 5 -> 4502 rounds to 4500
		{4503, 4505},   // tick 5
		{20007, 20010}, // tick 10 (<=30000 band)
	}
	for _, tc := range cases {
		if got := RoundToTickOf("9999", tc.in); got != tc.want {
			t.Fatalf("RoundToTickOf(9999, %g) = %g, want %g", tc.in, got, tc.want)
		}
	}
}

func TestIsTickAlignedOfCoarseBands(t *testing.T) {
	if !IsTickAlignedOf("9999", 2503) {
		t.Fatal("2503 should be tick-aligned (tick 1)")
	}
	if IsTickAlignedOf("9999", 4502) {
		t.Fatal("4502 should NOT be tick-aligned (tick 5)")
	}
	if !IsTickAlignedOf("9999", 4505) {
		t.Fatal("4505 should be tick-aligned (tick 5)")
	}
}

func TestTicker_SpreadTicks(t *testing.T) {
	tk := Ticker{Bid: 2500, Ask: 2502}
	if got := tk.SpreadTicks(1); got != 2 {
		t.Fatalf("SpreadTicks = %g, want 2", got)
	}
	if got := tk.SpreadTicks(0); got != 0 {
		t.Fatalf("SpreadTicks(0) = %g, want 0", got)
	}
}

func TestAggregator_RollsCandles(t *testing.T) {
	base := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)
	a := NewAggregator("7203", 10, time.Minute)
	a.OnTick(Ticker{Symbol: "7203", Bid: 2500, Ask: 2502}, base)
	a.OnTick(Ticker{Symbol: "7203", Bid: 2504, Ask: 2506}, base.Add(10*time.Second))
	// new minute -> previous candle rolls into the buffer
	a.OnTick(Ticker{Symbol: "7203", Bid: 2510, Ask: 2512}, base.Add(70*time.Second))

	candles := a.Candles(time.Minute)
	if len(candles) != 2 {
		t.Fatalf("want 2 candles (1 completed + 1 live), got %d", len(candles))
	}
	first := candles[0]
	if first.Open != 2501 || first.High != 2505 || first.Low != 2501 {
		t.Fatalf("first candle OHLC unexpected: %+v", first)
	}
	if last := a.Last(); last.Bid != 2510 {
		t.Fatalf("Last bid = %g, want 2510", last.Bid)
	}
}

func TestRingBuffer_DropsOldest(t *testing.T) {
	b := NewRingBuffer(3)
	for i := 0; i < 5; i++ {
		b.Push(Candle{Close: float64(i)})
	}
	snap := b.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("len = %d, want 3", len(snap))
	}
	if snap[0].Close != 2 || snap[2].Close != 4 {
		t.Fatalf("snapshot order wrong: %v", []float64{snap[0].Close, snap[1].Close, snap[2].Close})
	}
}

// 🚨 **呼値 0.1 の格子点を broker へ送れる文字列のまま返す**。
//
// roundTo は `math.Round(price/ts) * ts` で、0.1 は二進で表現できないので掛け戻した
// 瞬間に 1 ULP ずれる。adapter の ftoa は
// `strconv.FormatFloat(v,'f',-1,64)`(最短往復表現)なので、そのずれが
// **17 桁の注文値段**として立花へ出て行き、「逆指値条件に誤りがあります」で
// 守りの発注ごと拒否される。約定済みの建玉が守り無しで残る =
// CLAUDE.md「TP/SL は必ず broker 側」が破れる。
//
// 実際に起きた形(live トラック・9501・
// donchian_breakout_v2_trail): 板の逆指値が拒否され、約定済みの建玉が巻き戻った。
// 🛑 同じ日に板で受理されていた 8604(SL 1568)/ 9503(SL 2796)は呼値 0.5 で
// **文字列がぴったり**だった。stop-only('1')の形そのものは受理されていて、
// 違いは呼値だけ = 自然実験になっている(broker への問い合わせ無しで原因が確定する)。
//
// 🛑 丸めは adapter ではなく **domain のここだけ**でやる(position/price.go の設計)。
// ftoa 側で桁を丸めると、格子に載っていない値段(510.23)を黙って 510.2 に
// すり替える fail-open になる。
func TestRoundToTickOf_SubYenTickStaysOnGrid(t *testing.T) {
	cases := []struct {
		name   string
		symbol string
		price  float64
		want   float64
		wantS  string
	}{
		{
			// 実際に拒否された値。ATR(14)=22.24999999999999 → 2×ATR=44.49999999999998。
			name: "9501 の実測値(建値 554.7 − 2×ATR)", symbol: "9501",
			price: 554.7 - 44.49999999999998, want: 510.2, wantS: "510.2",
		},
		{
			// 🚨 **入力が既に格子の上でも壊れる**。roundTo は格子点に対して恒等ではなかった。
			name: "既に格子の上にある値を壊さない", symbol: "9501",
			price: 510.2, want: 510.2, wantS: "510.2",
		},
		{name: "9432 を上へ丸める", symbol: "9432", price: 175.66, want: 175.7, wantS: "175.7"},
		{name: "8729 を下へ丸める", symbol: "8729", price: 100.14, want: 100.1, wantS: "100.1"},
		{name: "境界 1000 円は細刻み側(0.1)", symbol: "7203", price: 1000, want: 1000, wantS: "1000"},

		// 以下は 0.5 / 5 円刻み = 二進で厳密。**壊れていない経路を固定する**。
		{name: "0.5 円刻み(細刻み・1000 円超)", symbol: "7203", price: 1000.3, want: 1000.5, wantS: "1000.5"},
		{name: "0.5 円刻み(細刻み・上側)", symbol: "7203", price: 2921.3, want: 2921.5, wantS: "2921.5"},
		{name: "粗いテーブル 5 円刻み", symbol: "9999", price: 4503, want: 4505, wantS: "4505"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RoundToTickOf(c.symbol, c.price)
			if got != c.want {
				t.Errorf("RoundToTickOf(%q, %v) = %v, want %v", c.symbol, c.price, got, c.want)
			}
			// 🛑 float の一致だけでは足りない。**broker へ出て行くのは文字列**で、
			// adapter は最短往復表現で書き出す。ここを見ないと 1 ULP のずれが素通りする。
			if s := strconv.FormatFloat(got, 'f', -1, 64); s != c.wantS {
				t.Errorf("broker へ送る文字列 = %q, want %q", s, c.wantS)
			}
		})
	}
}

// 呼値 0.1 の帯(100〜1,000 円)の格子点を全数で通す。実測では 3,243/9,001 = 36.0% が
// 壊れていた。1 点でも壊れたら「たまたま通った」だけなので、代表値ではなく全数で固定する。
func TestRoundToTickOf_FineGridIsExhaustivelyClean(t *testing.T) {
	for n := 1000; n <= 10000; n++ {
		p := float64(n) / 10
		got := RoundToTickOf("9501", p)
		gotS := strconv.FormatFloat(got, 'f', -1, 64)
		if wantS := strconv.FormatFloat(p, 'f', -1, 64); gotS != wantS {
			t.Fatalf("n=%d: RoundToTickOf(9501, %v) = %q, want %q", n, p, gotS, wantS)
		}
	}
}
