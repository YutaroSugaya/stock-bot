package market_test

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/market"
)

func tickAt(sym string, price, vol float64, ts time.Time) market.Ticker {
	return market.Ticker{Symbol: sym, Bid: price, Ask: price, Last: price, Volume: vol, Timestamp: ts}
}

// 🛑 broker が返すのは**当日の累計**出来高。そのままバーに入れると「5分足の出来高」が
// 「その時刻までの累計」になり、25日平均との比較が成立しなくなる。積むのは**増分**。
func TestAggregator_AccumulatesVolumeIncrements(t *testing.T) {
	base := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	a := market.NewAggregator("7203", 10, time.Minute)

	// 寄り付き直後は前回値が無いので、最初のバーは累計そのもの(= 寄りからの合計)。
	a.OnTick(tickAt("7203", 1000, 5000, base), base)
	a.OnTick(tickAt("7203", 1001, 8000, base.Add(10*time.Second)), base.Add(10*time.Second))

	bars := a.Candles(time.Minute)
	if len(bars) != 1 {
		t.Fatalf("bars = %d", len(bars))
	}
	if bars[0].Volume != 8000 {
		t.Fatalf("1本目の出来高 = %v, want 8000(寄りからの累計)", bars[0].Volume)
	}

	// 次のバーは**そのバーの中で増えた分**だけ。
	next := base.Add(time.Minute)
	a.OnTick(tickAt("7203", 1002, 9500, next), next)
	a.OnTick(tickAt("7203", 1003, 12000, next.Add(20*time.Second)), next.Add(20*time.Second))

	bars = a.Candles(time.Minute)
	if len(bars) != 2 {
		t.Fatalf("bars = %d", len(bars))
	}
	if bars[1].Volume != 4000 {
		t.Fatalf("2本目の出来高 = %v, want 4000(12000 − 8000)", bars[1].Volume)
	}
	if bars[0].Volume != 8000 {
		t.Fatalf("確定した1本目が書き換わっている: %v", bars[0].Volume)
	}
}

// 日跨ぎで累計がリセットされる。増分が負になったら**累計そのもの**を採る
// (負の出来高を書かない)。
func TestAggregator_VolumeResetAcrossDays(t *testing.T) {
	base := time.Date(2026, 8, 17, 14, 59, 0, 0, time.UTC)
	a := market.NewAggregator("7203", 10, time.Minute)
	a.OnTick(tickAt("7203", 1000, 900000, base), base)

	next := base.Add(time.Minute)
	a.OnTick(tickAt("7203", 1000, 1200, next), next) // 翌日の寄り(累計リセット)
	bars := a.Candles(time.Minute)
	if bars[len(bars)-1].Volume != 1200 {
		t.Fatalf("リセット後 = %v, want 1200(負の増分を書かない)", bars[len(bars)-1].Volume)
	}
}

// 出来高を返さない broker(paper / 旧配線)では 0 のまま。**0 と「取れなかった」を
// 混同しないための最低条件** — 存在しない出来高を捏造しない。
func TestAggregator_NoVolumeStaysZero(t *testing.T) {
	base := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	a := market.NewAggregator("7203", 10, time.Minute)
	a.OnTick(market.Ticker{Symbol: "7203", Bid: 1000, Ask: 1000, Last: 1000, Timestamp: base}, base)
	bars := a.Candles(time.Minute)
	if bars[0].Volume != 0 {
		t.Fatalf("出来高 = %v, want 0", bars[0].Volume)
	}
}
