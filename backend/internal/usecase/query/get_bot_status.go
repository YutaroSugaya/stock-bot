package query

import (
	"context"

	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// BotStatusView is the read DTO behind /api/status.
type BotStatusView struct {
	Mode             string                      `json:"mode"`
	BrokerKind       string                      `json:"broker_kind"`
	EmergencyStop    bool                        `json:"emergency_stop"`
	EmergencyReason  string                      `json:"emergency_reason,omitempty"`
	AccountOpenCount int                         `json:"account_open_count"`
	PerSymbol        map[string]SymbolStatusView `json:"per_symbol"`
}

type SymbolStatusView struct {
	ActiveConfigID string             `json:"active_config_id"`
	OpenPositions  []OpenPositionView `json:"open_positions"`
}

type EmergencyReader interface {
	Active() bool
	Reason() string
}

func idsOf(ps []position.Position) []int64 {
	out := make([]int64, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

type GetBotStatus struct {
	posRepo    port.PositionRepository
	emergency  EmergencyReader
	mode       string
	brokerKind string
	// nil のときは Strategy を空のままにする — 「unknown」で埋めて実在する戦略名の
	// ように見せない(UI は config_id へ落ちる)。
	strategies port.TradeStrategyResolver
}

func (q *GetBotStatus) WithStrategyResolver(sr port.TradeStrategyResolver) *GetBotStatus {
	q.strategies = sr
	return q
}

func NewGetBotStatus(pr port.PositionRepository, em EmergencyReader, mode, brokerKind string) *GetBotStatus {
	return &GetBotStatus{posRepo: pr, emergency: em, mode: mode, brokerKind: brokerKind}
}

func (q *GetBotStatus) Execute(ctx context.Context, activeConfigBySymbol map[string]string) (BotStatusView, error) {
	view := BotStatusView{
		Mode: q.mode, BrokerKind: q.brokerKind,
		PerSymbol: make(map[string]SymbolStatusView, len(activeConfigBySymbol)),
	}
	if q.emergency != nil {
		view.EmergencyStop = q.emergency.Active()
		view.EmergencyReason = q.emergency.Reason()
	}
	if n, err := q.posRepo.CountOpenAllSymbols(ctx); err == nil {
		view.AccountOpenCount = n
	}
	var ids []int64
	for symbol, cfgID := range activeConfigBySymbol {
		positions, err := q.posRepo.ListOpenOrClosing(ctx, symbol)
		if err != nil {
			return view, err
		}
		views := make([]OpenPositionView, 0, len(positions))
		for _, p := range positions {
			views = append(views, toView(p))
		}
		view.PerSymbol[symbol] = SymbolStatusView{ActiveConfigID: cfgID, OpenPositions: views}
		ids = append(ids, idsOf(positions)...)
	}
	// 戦略名は 1 回のクエリでまとめて解決する(銘柄ごとに引くと N+1 になる)。
	if q.strategies != nil && len(ids) > 0 {
		names, err := q.strategies.StrategyByPositionID(ctx, ids)
		if err != nil {
			return view, err
		}
		for sym, ss := range view.PerSymbol {
			for i := range ss.OpenPositions {
				if n := names[ss.OpenPositions[i].ID]; n != "" {
					ss.OpenPositions[i].Strategy = n
				} else {
					// 隠すより見える方が監査になる(forward-report と同じ規約)。
					ss.OpenPositions[i].Strategy = "unknown"
				}
			}
			view.PerSymbol[sym] = ss
		}
	}
	return view, nil
}
