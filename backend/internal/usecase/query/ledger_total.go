package query

import (
	"context"
	"sort"
	"time"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// LedgerTotalView は台帳の累計(決済の net + 建玉の含み)。ダッシュボードの累計欄がこれを出す。
//
// 決済の net は台帳と同じ規約(gross − fee + carry。carry は負で入る・forward_report と同じ)。
// 含みは時価がある建玉だけ入れ、無い銘柄は Unpriced に名指しする(黙って 0 にすると「戻った」に見える)。
type LedgerTotalView struct {
	RealizedNetJPY float64  `json:"realized_net_jpy"`
	UnrealizedJPY  float64  `json:"unrealized_jpy"`
	TotalJPY       float64  `json:"total_jpy"`
	ClosedN        int      `json:"closed_n"`
	OpenN          int      `json:"open_n"`
	PricedN        int      `json:"priced_n"`
	Unpriced       []string `json:"unpriced,omitempty"`
}

type LedgerTotal struct {
	trades    port.ClosedTradeReader
	positions port.PositionRepository
}

func NewLedgerTotal(trades port.ClosedTradeReader, positions port.PositionRepository) *LedgerTotal {
	return &LedgerTotal{trades: trades, positions: positions}
}

// Execute は全期間の決済と今の建玉を合算する。lastPrice は銘柄の直近の約定値(無ければ false)。
func (q *LedgerTotal) Execute(ctx context.Context, lastPrice func(symbol string) (float64, bool)) (LedgerTotalView, error) {
	var v LedgerTotalView
	closed, err := q.trades.ListClosedSince(ctx, time.Time{})
	if err != nil {
		return v, err
	}
	for _, t := range closed {
		v.RealizedNetJPY += t.ProfitLossJPY - t.FeeJPY + t.CarryJPY
	}
	v.ClosedN = len(closed)
	open, err := q.positions.ListOpenAllSymbols(ctx)
	if err != nil {
		return v, err
	}
	v.OpenN = len(open)
	for _, p := range open {
		px, ok := lastPrice(p.Symbol)
		if !ok || px <= 0 {
			v.Unpriced = append(v.Unpriced, p.Symbol)
			continue
		}
		sign := 1.0
		if p.Side == order.SideSell {
			sign = -1.0
		}
		v.UnrealizedJPY += sign * (px - p.EntryPrice) * float64(p.Quantity)
		v.PricedN++
	}
	sort.Strings(v.Unpriced)
	v.TotalJPY = v.RealizedNetJPY + v.UnrealizedJPY
	return v, nil
}
