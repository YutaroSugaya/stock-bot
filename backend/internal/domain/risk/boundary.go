package risk

import (
	"stockbot/backend/internal/domain/strategy"
)

// 1トレードあたりの fat-finger 天井。**戦略の幾何を整形するためではなく**桁違いの誤り(10倍ロット・
// 50% ストップ)だけを捕まえる絶対的な backstop なので、広いストップの多日保有戦略は通る側に緩く置く。
// 0 はそのチェックを無効化する。
type OrderBoundary struct {
	// 建値に対する %。円や tick 数では価格帯ごとに意味が変わるので経済的な上限は % で持つ。
	MaxStopLossPct     float64
	MaxTakeProfitPct   float64
	MaxLossPerTradeJPY int
}

// 出口幅は円/株なので最悪損失は 幅 × 株数。%上限は建値比。gate の後・saga の前に走る hard gate。
func EvaluateOrderBoundary(sig strategy.Signal, qty int, b OrderBoundary) Decision {
	if sig.EntryPrice > 0 {
		if b.MaxStopLossPct > 0 && sig.StopLossJPY/sig.EntryPrice*100 > b.MaxStopLossPct {
			return Decision{Allowed: false, Reason: "stop_loss_pct_exceeds_boundary"}
		}
		if b.MaxTakeProfitPct > 0 && sig.TakeProfitJPY/sig.EntryPrice*100 > b.MaxTakeProfitPct {
			return Decision{Allowed: false, Reason: "take_profit_pct_exceeds_boundary"}
		}
	}
	if b.MaxLossPerTradeJPY > 0 {
		worstLoss := sig.StopLossJPY * float64(qty)
		if worstLoss > float64(b.MaxLossPerTradeJPY) {
			return Decision{Allowed: false, Reason: "max_loss_per_trade_exceeded"}
		}
	}
	return Decision{Allowed: true}
}
