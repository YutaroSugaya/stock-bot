package backtest

import (
	"time"

	"stockbot/backend/internal/domain/order"
)

// Trade is one closed round-trip. Gross and net are both kept so the cost
// floor stays visible: net = gross - fee + carry.
type Trade struct {
	Symbol      string     `json:"symbol"`
	Side        order.Side `json:"side"`
	EntryPrice  float64    `json:"entry_price"`
	ExitPrice   float64    `json:"exit_price"`
	Quantity    int        `json:"quantity"`
	OpenedAt    time.Time  `json:"opened_at"`
	ClosedAt    time.Time  `json:"closed_at"`
	GrossJPY    float64    `json:"gross_jpy"`
	FeeJPY      float64    `json:"fee_jpy"`
	CarryJPY    float64    `json:"carry_jpy"`
	NetJPY      float64    `json:"net_jpy"`
	CloseReason string     `json:"close_reason"`
}

type Metrics struct {
	N               int     `json:"n"`
	GrossPF         float64 `json:"gross_pf"`
	NetPF           float64 `json:"net_pf"`
	GrossExpectancy float64 `json:"gross_expectancy_jpy"`
	NetExpectancy   float64 `json:"net_expectancy_jpy"`
	WinRate         float64 `json:"win_rate"`
	AvgWinJPY       float64 `json:"avg_win_jpy"`
	AvgLossJPY      float64 `json:"avg_loss_jpy"`
	RR              float64 `json:"rr"`
	MaxConsecLoss   int     `json:"max_consec_loss"`
	MaxDrawdownJPY  float64 `json:"max_drawdown_jpy"`
}

type EquityPoint struct {
	Time   time.Time `json:"time"`
	Equity float64   `json:"equity"`
}

type Result struct {
	Symbol        string        `json:"symbol"`
	Trades        []Trade       `json:"trades"`
	Metrics       Metrics       `json:"metrics"`
	Equity        []EquityPoint `json:"equity"`
	AmbiguousBars int           `json:"ambiguous_bars"` // bars where TP and SL both touched
}
