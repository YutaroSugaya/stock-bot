package strategy

import "stockbot/backend/internal/domain/order"

// 提案 entry が越えるべき往復コスト床(tick 建て)。intraday は carry 0、multiday は carry を足す。
type CostFloor struct {
	SpreadTicks     float64
	SlippageTicks   float64
	CarryTicks      float64 // multiday only; 0 for intraday
	MinEdgeMultiple float64 // 利確幅は floor × これ 以上でなければならない(backtest 較正)
	// tick 建ての床を円へ直す呼値。0 = 呼値不明なので床の比較をしない。
	TickSize float64
}

func (c CostFloor) RoundTripTicks(mode order.HoldingMode) float64 {
	floor := c.SpreadTicks + c.SlippageTicks
	if mode == order.HoldingMultiday {
		floor += c.CarryTicks
	}
	return floor
}

// RoundTripJPY is the round-trip cost floor in 円/株 (呼値 × tick 数)。
func (c CostFloor) RoundTripJPY(mode order.HoldingMode) float64 {
	return c.RoundTripTicks(mode) * c.TickSize
}

// TickSize<=0 は床を円に直せないので比較しない。MinEdgeMultiple<=0 は 1.0 扱い(設定ミスで全通しにしない)。
func (c CostFloor) ClearsFloor(tpJPY float64, mode order.HoldingMode) bool {
	if c.TickSize <= 0 {
		return true
	}
	mult := c.MinEdgeMultiple
	if mult <= 0 {
		mult = 1.0
	}
	return tpJPY >= c.RoundTripJPY(mode)*mult
}

// ReasonTPBelowCostFloor は「入口は成立したのに、狙える利幅が往復コストの床に
// 届かないので我々が断った」見送り。
//
// 🛑 **これは「セットアップが無い」とは別物**で、usecase 側が signal_rejections に
// 記録する数少ない見送り理由。床の対象は
// trail 側だけ 1.0×ATR = v2 の 3 倍厳しいので、**壊れたペアは選択的に trail 側から
// 消える**。件数を実時間で書かないと、締めのときには再構成できない。
// 文字列を 2 箇所に書かないためにここが唯一の正本。
const ReasonTPBelowCostFloor = "tp_below_cost_floor"

// uncapped な trail(TP=0 / ratchet>0)は ratchet arm 距離を最小の利益目標として床に当てる
// — 0 と比べると全ての trail entry が落ちる。
func applyCostFloor(in EvalInput, sig Signal, floor CostFloor) Signal {
	if sig.Decision != DecisionEnter {
		return sig
	}
	target := sig.TakeProfitJPY
	if sig.RatchetArmJPY > target {
		target = sig.RatchetArmJPY
	}
	if !floor.ClearsFloor(target, sig.HoldingMode) {
		return noTradeSignal(in, sig.StrategyName, ReasonTPBelowCostFloor)
	}
	return sig
}
