package market

import "math"

// ExDateSplitRatio は**権利落ち日の値段**から株式分割(併合)を断定し、**株数の倍率**
// (1:5 分割なら 5、10:1 併合なら 0.1)を返す。
//
// 🚨 なぜ要るか(paper で踏んだ 1:5 分割の例): 建玉は建値・TP・SL を**分割前の円**で凍結して
// いるので、権利落ちの寄り(12,595 → 2,495)で SL 11,5xx 円を「割った」と読み、見かけ上の
// 大きな損切りを台帳へ書いた。実際は 500 株・建値 2,314 円相当の**含み益**だった。
//
// 判定は 2 つの独立な証拠の両方を要る:
//  1. **price が prevClose を基準値段にした値幅制限の外** — 実相場では約定し得ない値段で、
//     取引所が基準値段を権利落ち分だけ動かした証拠。これが無い限り、比が単純分割比に
//     似ていても**本物の暴落**として扱う(読み違えると SL を割り引いて守りが効かなくなる)。
//  2. **比が単純分割比(split_guard.go の表)に一致** — 帯の外でも半端な比はデータ異常として
//     捨てる(断定しない = 従来どおりの扱い)。
//
// 1 があるので、日足の chain-link(SplitRatio)が実暴落と区別できずに捨てている
// 下落方向の浅い比(1:1.5)もここでは拾う。
//
// ⚠ 連続ストップ時の値幅拡大(price_limit.go 冒頭)は実装していないので、拡大日の実暴落が
// 帯の外に見える余地はある。その場合も 2 を満たさなければ分割とは読まない。
// prevClose は**分割未調整**の前営業日終値であること(運用側の日足の最終バーがそれ)。
func ExDateSplitRatio(prevClose, price float64) (float64, bool) {
	if !(prevClose > 0) || !(price > 0) || math.IsInf(prevClose, 0) || math.IsInf(price, 0) {
		return 0, false
	}
	lo, okLo := LimitDown(prevClose)
	hi, okHi := LimitUp(prevClose)
	if !okHi {
		return 0, false
	}
	// 🛑 帯の端(ストップ安/高ちょうど)は**内側**。等値は float 誤差があるので許容幅で比べる。
	const eps = 1e-6
	outside := price > hi+eps || (okLo && price < lo-eps)
	if !outside {
		return 0, false
	}
	cand, ok := nearestSimpleRatio(price/prevClose, true)
	if !ok {
		return 0, false
	}
	return 1 / cand, true
}
