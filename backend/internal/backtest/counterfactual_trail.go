package backtest

import (
	"time"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

// TrailCounterfactual は **トレール建玉の反実仮想**。
//
// 🚨 なぜ固定 TP/SL の `Counterfactual` では足りないか: トレール建玉の実際の出口は
// `floor = max(arm, peak − giveback)` の線であって固定 TP ではない。固定脚で歩いた
// 結果は**別戦略の推定**なので、08-13 の格子(arm × giveback を後掛けした先行調査)と
// 実アームを突き合わせたときの不一致を「反実仮想の推定誤差」と誤読する。
//
// 🛑 **床の版スタンプ(`FloorAtArm`)を尊重する。** false の建玉(旧台帳の
// 旧建玉)に床を当て直して読むのは、測っている対象を後から入れ替えること。
// 呼び手が凍結値をそのまま渡す。
//
// 🛑 **日足で歩くので日中の順序は分からない。** 同じバーで arm と床の両方に触れうるが、
// どちらが先かは決められない。**armed になったバーでは床で出さない**(その日の高値で
// armed → 同日中に返す、という最も有利な順序を勝手に採らないため。悲観側に倒す)。
type TrailCFInput struct {
	Side        order.Side
	ArmJPY      float64 // ratchet_arm_jpy(円/株)
	GivebackJPY float64 // ratchet_giveback_jpy(円/株)
	StopLossJPY float64 // SL 価格(0 = SL 無し)。トレールでも SL は broker 側にある
	FloorAtArm  bool    // 床が効く建玉か(凍結値)
	EntryPrice  float64
	// PeakAtClose は**決済時点までに既に到達していた** MFE(円/株)。打ち切った建玉の
	// 続きを歩くので、peak はゼロから数え直さない。
	PeakAtClose float64
	Armed       bool // 決済時点で既に armed だったか
	After       time.Time
	Until       time.Time // 凍結 MaxHold の期限(zero = キャップ無し)
	Bars        []market.Candle
}

// TrailCFOutcome は決着の種類。
type TrailCFOutcome string

const (
	TrailCFFloor      TrailCFOutcome = "ratchet_floor" // 床に触れて利確 / giveback
	TrailCFStopLoss   TrailCFOutcome = "stop_loss"
	TrailCFMaxHold    TrailCFOutcome = "max_hold"
	TrailCFUnresolved TrailCFOutcome = "unresolved"
	TrailCFNoData     TrailCFOutcome = "no_data"
)

type TrailCFResult struct {
	Outcome TrailCFOutcome
	HitDate time.Time
	Days    int
	// ExitUnrealJPY は決着時の含み(円/株・side 補正済み)。gross = これ × 数量。
	ExitUnrealJPY float64
	// PeakJPY は歩き終えた時点の MFE(円/株)。床がどこまで上がったかを読む材料。
	PeakJPY float64
	// Armed は歩いている途中で armed に到達したか。
	Armed bool
}

// TrailCounterfactual walks the bars after the close under the TRAIL exit rule.
func TrailCounterfactual(in TrailCFInput) TrailCFResult {
	peak, armed, n := in.PeakAtClose, in.Armed, 0
	var last float64
	for _, b := range in.Bars {
		if !b.OpenTime.After(in.After) {
			continue
		}
		n++
		// 期限を過ぎた最初のバーは**寄りで成行返済**(Counterfactual と同じ規約)。
		if !in.Until.IsZero() {
			day, cap := barDay(b.OpenTime), barDay(in.Until)
			if day.After(cap) {
				return TrailCFResult{Outcome: TrailCFMaxHold, HitDate: b.OpenTime, Days: n,
					ExitUnrealJPY: unrealAt(in.Side, in.EntryPrice, b.Open), PeakJPY: peak, Armed: armed}
			}
		}
		hi, lo := unrealAt(in.Side, in.EntryPrice, b.High), unrealAt(in.Side, in.EntryPrice, b.Low)
		best, worst := hi, lo
		if in.Side == order.SideSell { // 売りは安値が有利side
			best, worst = lo, hi
		}
		// SL は床より先に見る(守りは broker 側にあり、床は bot の OnTick)。
		if in.StopLossJPY > 0 {
			sl := unrealAt(in.Side, in.EntryPrice, in.StopLossJPY)
			if worst <= sl {
				return TrailCFResult{Outcome: TrailCFStopLoss, HitDate: b.OpenTime, Days: n,
					ExitUnrealJPY: sl, PeakJPY: peak, Armed: armed}
			}
		}
		wasArmed := armed
		if best > peak {
			peak = best
		}
		if !armed && peak >= in.ArmJPY {
			armed = true
		}
		// 🛑 **armed になったバーでは床で出さない**(日中の順序が決められないので、
		// 「高値で armed → 同日中に返す」という最も有利な読みを採らない)。
		if armed && wasArmed {
			floor := peak - in.GivebackJPY
			if in.FloorAtArm && floor < in.ArmJPY {
				floor = in.ArmJPY
			}
			if worst <= floor {
				return TrailCFResult{Outcome: TrailCFFloor, HitDate: b.OpenTime, Days: n,
					ExitUnrealJPY: floor, PeakJPY: peak, Armed: true}
			}
		}
		last = unrealAt(in.Side, in.EntryPrice, b.Close)
		if !in.Until.IsZero() && barDay(b.OpenTime).Equal(barDay(in.Until)) {
			return TrailCFResult{Outcome: TrailCFMaxHold, HitDate: b.OpenTime, Days: n,
				ExitUnrealJPY: last, PeakJPY: peak, Armed: armed}
		}
	}
	if n == 0 {
		return TrailCFResult{Outcome: TrailCFNoData}
	}
	return TrailCFResult{Outcome: TrailCFUnresolved, ExitUnrealJPY: last, PeakJPY: peak, Armed: armed}
}

// unrealAt は side 補正済みの含み(円/株)。SELL は符号が反転する。
func unrealAt(side order.Side, entry, price float64) float64 {
	if side == order.SideSell {
		return entry - price
	}
	return price - entry
}
