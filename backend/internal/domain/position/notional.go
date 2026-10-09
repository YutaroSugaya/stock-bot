package position

import "math"

// Per1MNotional は円の損益を「建玉金額 ¥1,000,000 あたり」へ直す(EDGE_METHODOLOGY の共通規約)。
// 建玉金額が違う銘柄の損益をそのまま平均すると、値がさ株の 1 件が全体を決める。
//
// 建玉金額 = 建値 × |株数|(売り建ての負数量も金額としては正)。出せないとき(建値 0・株数 0・
// NaN)は 0 を返す — 落とすか数えるかは呼び手が決める(0 円の建玉は存在しない)。
// forward-report / edge-eval / pair-diff は全部これを通る(写すと丸めの違いで判定器ごとに値がずれる)。
func Per1MNotional(jpy, entryPrice float64, qty int) float64 {
	if qty < 0 {
		qty = -qty
	}
	notional := entryPrice * float64(qty)
	if notional <= 0 || math.IsNaN(notional) {
		return 0
	}
	return jpy / notional * 1e6
}
