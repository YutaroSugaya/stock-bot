package main

import (
	"stockbot/backend/internal/app"
	"stockbot/backend/internal/domain/market"
)

// quote_view.go shapes the dashboard's per-symbol quote entry.
//
// 終日一度も約定しない銘柄(立花の現在値=0 → 前日終値フォールバック)は
// tradeable な summary が立たず **ダッシュボードから丸ごと消えていた**(実測 6976 /
// 6981 / 285A が建玉を持ったまま不可視で、売買停止とフィード故障を区別できなかった)。
//
// ただし前日終値を `last` に流し込んで埋めてはいけない。含み損益・株価差・TP までの
// 距離は全部 `last` から組み上がるので、動いていない値段の嘘の損益が画面に出る。
// indicative は **別キー**(stale / prev_close / quote_bid / quote_ask)で出す。

// quoteViews builds the `summaries` payload. Symbols that have never produced any
// quote are omitted.
func quoteViews(bundles []*app.SymbolBundle) map[string]any {
	out := make(map[string]any, len(bundles))
	for _, b := range bundles {
		if v := quoteView(b.LastSummary(), b.LastIndicative()); v != nil {
			out[b.Symbol] = v
		}
	}
	return out
}

// quoteView builds ONE symbol's entry from its last tradeable quote and, when the
// tape is frozen, its last indicative one. Either may be nil; both nil yields nil.
func quoteView(tradeable, indicative *market.MarketSummary) map[string]any {
	if tradeable == nil && indicative == nil {
		return nil
	}
	v := map[string]any{}
	if s := tradeable; s != nil {
		v["bid"] = s.CurrentRate.Bid
		v["ask"] = s.CurrentRate.Ask
		v["last"] = s.CurrentRate.Last // 約定した値段だけ。indicative は入れない
		v["spread_ticks"] = s.CurrentRate.SpreadTicks
		v["tick_size"] = s.TickSize
	}
	if s := indicative; s != nil {
		v["stale"] = true
		v["prev_close"] = s.CurrentRate.Last // = 前日終値(立花の pPRP フォールバック)
		v["quote_bid"] = s.CurrentRate.Bid
		v["quote_ask"] = s.CurrentRate.Ask
		v["stale_at"] = s.GeneratedAt
		if _, ok := v["tick_size"]; !ok {
			v["tick_size"] = s.TickSize
		}
	}
	return v
}
