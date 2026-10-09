package strategy

import (
	"sort"

	"stockbot/backend/internal/config"
)

// rand を domain に持ち込まないための注入点(engine を純粋に保ち backtest を決定的にする)。
type SignalIDFn func() string

type Engine struct {
	registry map[config.StrategyName]Strategy
	signalID SignalIDFn
}

// no_trade は常に fail-safe 既定として登録する。
func NewEngine(signalID SignalIDFn, strategies ...Strategy) *Engine {
	reg := map[config.StrategyName]Strategy{
		config.StrategyNoTrade: NoTrade{},
	}
	for _, s := range strategies {
		reg[s.Name()] = s
	}
	if signalID == nil {
		signalID = func() string { return "" }
	}
	return &Engine{registry: reg, signalID: signalID}
}

// 起動時に未登録戦略を loud に落とすため(runtime で黙って NO_TRADE に縮退させない)。
func (e *Engine) Has(name config.StrategyName) bool {
	_, ok := e.registry[name]
	return ok
}

func (e *Engine) Names() []string {
	out := make([]string, 0, len(e.registry))
	for n := range e.registry {
		out = append(out, string(n))
	}
	sort.Strings(out)
	return out
}

// config 無し / 期限切れ / 未知戦略はいずれも NO_TRADE(fail-safe)。
func (e *Engine) Evaluate(in EvalInput) Signal {
	if in.Config == nil {
		return Signal{Decision: DecisionNoTrade, Reason: "no_active_config", CreatedAt: in.Now}
	}
	if in.Config.IsExpired(in.Now) {
		return noTradeSignal(in, in.Config.StrategyName, "config_expired")
	}
	strat, ok := e.registry[in.Config.StrategyName]
	if !ok {
		return noTradeSignal(in, in.Config.StrategyName, "unknown_strategy")
	}
	sig := strat.Evaluate(in)
	if sig.IsEntry() && sig.SignalID == "" {
		sig.SignalID = e.signalID()
	}
	return sig
}
