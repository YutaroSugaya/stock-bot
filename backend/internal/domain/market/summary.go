package market

import (
	"encoding/json"
	"time"
)

type MarketSummary struct {
	Symbol      string      `json:"symbol"`
	GeneratedAt time.Time   `json:"generated_at"`
	CurrentRate CurrentRate `json:"current_rate"`
	TickSize    float64     `json:"tick_size"`

	// LLM advisor が読む read-only の判断パケット(不透明 JSON)。決定的戦略は読まない。
	Advisory json.RawMessage `json:"advisory,omitempty"`

	// 戦略別ラウンドロビンで割り当てたスロットの戦略名。prompt が LLM に「この戦略か
	// no_trade のみ」と伝え Promoter が強制する。束縛が無いと、入口が他の部分集合になる戦略
	// (52週高値 ⊂ abs_momentum)は常に選び負けて forward 標本がゼロになる。
	SlotStrategy string `json:"slot_strategy,omitempty"`
}

type CurrentRate struct {
	Bid         float64   `json:"bid"`
	Ask         float64   `json:"ask"`
	Last        float64   `json:"last"`
	SpreadTicks float64   `json:"spread_ticks"`
	At          time.Time `json:"at"`
}

func SummaryFromTicker(t Ticker, now time.Time) *MarketSummary {
	ts := TickSizeOf(t.Symbol, t.Mid())
	return &MarketSummary{
		Symbol:      t.Symbol,
		GeneratedAt: now,
		TickSize:    ts,
		CurrentRate: CurrentRate{
			Bid:         t.Bid,
			Ask:         t.Ask,
			Last:        t.Last,
			SpreadTicks: t.SpreadTicks(ts),
			At:          t.Timestamp,
		},
	}
}
