package main

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/market"
)

func summaryAt(last, bid, ask float64, at time.Time) *market.MarketSummary {
	return &market.MarketSummary{
		Symbol: "7203", GeneratedAt: at, TickSize: 1,
		CurrentRate: market.CurrentRate{Bid: bid, Ask: ask, Last: last, SpreadTicks: ask - bid, At: at},
	}
}

func TestQuoteView(t *testing.T) {
	at := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)

	t.Run("tradeable", func(t *testing.T) {
		v := quoteView(summaryAt(2500, 2499, 2501, at), nil)
		if v["last"] != 2500.0 {
			t.Fatalf("last = %v, want 2500", v["last"])
		}
		if _, ok := v["stale"]; ok {
			t.Fatalf("約定している銘柄に stale が付いた: %+v", v)
		}
	})

	// 終日未約定(売買停止/気配のみ)。**last を出してはいけない**: 含み損益は last
	// から組むので、前日終値を入れると動いていない値段の嘘の損益が画面に出る。
	t.Run("indicative only", func(t *testing.T) {
		v := quoteView(nil, summaryAt(8723, 8400, 8450, at))
		if _, ok := v["last"]; ok {
			t.Fatalf("前日終値が last として出た(嘘の含み損益になる): %+v", v)
		}
		if v["stale"] != true {
			t.Fatalf("stale バッジが無い: %+v", v)
		}
		if v["prev_close"] != 8723.0 {
			t.Fatalf("prev_close = %v, want 8723", v["prev_close"])
		}
		if v["quote_bid"] != 8400.0 || v["quote_ask"] != 8450.0 {
			t.Fatalf("気配が出ていない: %+v", v)
		}
		if v["tick_size"] != 1.0 {
			t.Fatalf("tick_size = %v, want 1", v["tick_size"])
		}
	})

	// 前場は約定していて後場から停止: 最後に**約定した**値段(= 最後の実勢)は残しつつ、
	// 止まっていることも画面から読めること。
	t.Run("traded then halted", func(t *testing.T) {
		v := quoteView(summaryAt(2500, 2499, 2501, at), summaryAt(2500, 2300, 2310, at.Add(time.Hour)))
		if v["last"] != 2500.0 {
			t.Fatalf("最終約定値が消えた: %+v", v)
		}
		if v["stale"] != true || v["quote_bid"] != 2300.0 {
			t.Fatalf("停止中の気配が出ていない: %+v", v)
		}
	})

	t.Run("no quote at all", func(t *testing.T) {
		if v := quoteView(nil, nil); v != nil {
			t.Fatalf("一度も気配が来ていない銘柄は載せない: %+v", v)
		}
	})
}
