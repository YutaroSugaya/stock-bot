package position

import (
	"math"
	"time"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

// Exit=false かつ ExcursionChanged=true のとき、呼び手は NewPeak/NewTrough/NewArmed を
// 永続化する。**名前を RatchetChanged から変えたのは意味が変わったから** — peak/trough は
// ratchet を持たない建玉でも更新する(同じ名前で意味だけ変えると古い呼び出しが黙って
// 別物になる)。
type ExitDecision struct {
	Exit             bool
	Reason           string
	ExcursionChanged bool
	NewPeak          float64
	NewTrough        float64
	NewArmed         bool
}

// 建玉の決済判定(純粋)。live 実行 engine と backtest engine が同じ規則を共有するために domain に置く。
// 優先順位: ratchet トレール TP → 利確 → 損切 → 最大保有(延長込み)→ 早期撤退窓。
func EvaluateExit(p Position, price float64, now time.Time) ExitDecision {
	unreal := p.UnrealizedJPY(price)
	// MFE / MAE are tracked for every position; only the ratchet rule reads them,
	// so this is a measurement, not an exit rule.
	peak := math.Max(p.PeakUnrealizedJPY, unreal)
	trough := math.Min(p.TroughUnrealizedJPY, unreal)

	// Ratchet trailing take-profit takes precedence over fixed TP/SL.
	if p.RatchetArmJPY > 0 {
		armed := p.RatchetArmed || peak >= p.RatchetArmJPY
		if armed && p.RatchetGivebackJPY > 0 && unreal <= ratchetFloor(p, peak) {
			return withExcursion(ExitDecision{Exit: true, Reason: "ratchet_takeprofit"}, p, peak, trough, armed)
		}
		return withExcursion(evaluateExitAfterRatchet(p, price, now, unreal), p, peak, trough, armed)
	}
	return withExcursion(evaluateExitAfterRatchet(p, price, now, unreal), p, peak, trough, p.RatchetArmed)
}

// ratchetFloor は「これ以下の含みになったらトレールを利確する」線(円/株・符号付き)。
//
//	旧: floor = peak − giveback
//	新: floor = max(RatchetArmJPY, peak − giveback)     ← RatchetFloorAtArm の建玉のみ
//
// **armed になったら建値割れで出ない。** giveback(1.5×ATR)が arm(1.0×ATR)より
// 大きいので、旧規則では peak が 1.0〜1.5×ATR の帯で armed になった玉は**構造的に必ず
// 損で出る**(最悪 −0.5×ATR)。「トレール利確」を名乗る出口が建値割れで終わる構成は
// 名前と実装が矛盾している — 1 本も約定を見なくても指摘できる欠陥。
//
// peak ≥ arm+giveback では `peak − giveback` が勝つので**旧規則と完全に同じ**。
// 変わるのは peak が arm〜arm+giveback の帯だけ。パラメータは増えない(既存の 2 つを
// 組み合わせ直すだけ)。
//
// 🛑 判定は `unreal <= floor`(**触れたら出る**)。旧規則の `(peak-unreal) >= giveback`
// と同じ比較の向きで、armed と決済が同一ティックで起きうる(境界 (a))。
// 🛑 unreal / peak / floor はすべて **side 補正済みの円/株**。SELL 建玉でも同じ式が
// 成り立つ(価格の大小で書き直すと空売りで反転する)。
func ratchetFloor(p Position, peak float64) float64 {
	floor := peak - p.RatchetGivebackJPY
	if p.RatchetFloorAtArm && floor < p.RatchetArmJPY {
		return p.RatchetArmJPY
	}
	return floor
}

// TrailFloorStopPrice は trail の利確の線(ratchetFloor)を**価格**にしたもの(呼値に丸める)。
// 板の SL をここまで引き上げるのに使う。ok=false = 線が無い
// (ratchet を持たない建玉 / まだ arm していない建玉)= 引き上げない。arm の判定は Evaluate と同じ。
func TrailFloorStopPrice(p Position) (price float64, ok bool) {
	if p.RatchetArmJPY <= 0 || p.RatchetGivebackJPY <= 0 {
		return 0, false
	}
	if !p.RatchetArmed && p.PeakUnrealizedJPY < p.RatchetArmJPY {
		return 0, false
	}
	floor := ratchetFloor(p, p.PeakUnrealizedJPY)
	if p.Side == order.SideSell {
		return market.RoundToTickOf(p.Symbol, p.EntryPrice-floor), true
	}
	return market.RoundToTickOf(p.Symbol, p.EntryPrice+floor), true
}

// withExcursion attaches the MFE/MAE record to a decision. It rides along on
// EXIT decisions too: the tick that hits the stop is the one that carries the
// worst excursion, and after the close it can no longer be observed.
// Changed=false when no extreme moved — OnTick runs every 3 seconds per position,
// so an unconditional "changed" would rewrite the same row all day.
func withExcursion(d ExitDecision, p Position, peak, trough float64, armed bool) ExitDecision {
	d.ExcursionChanged = peak != p.PeakUnrealizedJPY || trough != p.TroughUnrealizedJPY || armed != p.RatchetArmed
	d.NewPeak, d.NewTrough, d.NewArmed = peak, trough, armed
	return d
}

// 比較は建玉時に凍結した絶対価格(換算を挟まない)。0 = その脚が無いので評価しない
// — 0 を「即決済」と読むと建てた瞬間に閉じる。
func evaluateExitAfterRatchet(p Position, price float64, now time.Time, unreal float64) ExitDecision {
	if p.TakeProfitPrice > 0 && reachedTP(p.Side, price, p.TakeProfitPrice) {
		return ExitDecision{Exit: true, Reason: "take_profit"}
	}
	if p.StopLossPrice > 0 && reachedSL(p.Side, price, p.StopLossPrice) {
		return ExitDecision{Exit: true, Reason: "stop_loss"}
	}
	if p.MaxHoldMinutes > 0 {
		soft := p.MaxHoldUntil()
		if !now.Before(soft) {
			// Extension: hold past the soft deadline only if near break-even.
			if p.ExtensionMaxMinutes > 0 && math.Abs(unreal) <= p.ExtensionUnrealizedJPY {
				hard := soft.Add(time.Duration(p.ExtensionMaxMinutes) * time.Minute)
				if now.Before(hard) {
					return ExitDecision{}
				}
			}
			return ExitDecision{Exit: true, Reason: "max_hold"}
		}
		// Early-exit window before the soft deadline.
		if p.EarlyExitWindowMinutes > 0 {
			windowStart := soft.Add(-time.Duration(p.EarlyExitWindowMinutes) * time.Minute)
			if !now.Before(windowStart) && p.EarlyExitTargetJPY > 0 && unreal >= p.EarlyExitTargetJPY {
				return ExitDecision{Exit: true, Reason: "early_exit"}
			}
		}
	}
	return ExitDecision{}
}

// 損失側の守りが実際に効く値段と理由を返す(トレール建玉は giveback 線と SL の2本を持つ)。EvaluateExit が
// ratchet を先に見るのは「同じ tick で両方成立したらどちらを名乗るか」でしかなく、**遠い方の線には価格が
// 先に届かない** — 買いなら高い方、売りなら低い方が先に効く。domain に置くのは、ダッシュボードが同じ規則を
// JS で書き直すと EvaluateExit から静かにずれるため。(0, "") = 守りの脚が無い(外部採用建玉など)。
func ProtectiveExit(p Position) (price float64, reason string) {
	if p.RatchetArmJPY > 0 && p.RatchetArmed && p.RatchetGivebackJPY > 0 {
		// giveback 線: 床に対応する価格。**EvaluateExit と同じ ratchetFloor を
		// 通す** — 片方だけ旧式のままだと、画面と broker に出る守りの値段が実際の
		// 決済線と静かにずれる。
		line := p.PriceForUnrealized(ratchetFloor(p, p.PeakUnrealizedJPY))
		if p.StopLossPrice <= 0 || firstReachedOnAdverse(p.Side, line, p.StopLossPrice) {
			return line, "ratchet_takeprofit"
		}
	}
	if p.StopLossPrice > 0 {
		return p.StopLossPrice, "stop_loss"
	}
	return 0, ""
}

// 不利方向へ動いたとき a が b より先に届くか(買いは高い方、売りは低い方が先)。
func firstReachedOnAdverse(side order.Side, a, b float64) bool {
	if side == order.SideSell {
		return a < b
	}
	return a > b
}

// reachedTP / reachedSL: 買いは TP が上・SL が下、売りはその逆。
func reachedTP(side order.Side, price, tp float64) bool {
	if side == order.SideSell {
		return price <= tp
	}
	return price >= tp
}

func reachedSL(side order.Side, price, sl float64) bool {
	if side == order.SideSell {
		return price >= sl
	}
	return price <= sl
}
