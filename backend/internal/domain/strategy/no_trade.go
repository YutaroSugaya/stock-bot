package strategy

import "stockbot/backend/internal/config"

// 既定戦略。エッジ証明前はこれだけが配線され、bot はループを回すだけで何も発注しない。
type NoTrade struct{}

func (NoTrade) Name() config.StrategyName { return config.StrategyNoTrade }

func (NoTrade) Evaluate(in EvalInput) Signal {
	return noTradeSignal(in, config.StrategyNoTrade, "no_trade_default")
}
