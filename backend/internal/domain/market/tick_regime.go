package market

// TickRegime は銘柄が乗っている呼値テーブル(tick_fine.go 参照)。
type TickRegime int

const (
	// TickRegimeCoarse は通常の呼値テーブル(TickSize)。**証拠が無いときの既定**。
	TickRegimeCoarse TickRegime = iota
	// TickRegimeFine は細かい呼値テーブル(fineTickSize)。
	TickRegimeFine
)

func (r TickRegime) String() string {
	if r == TickRegimeFine {
		return "fine"
	}
	return "coarse"
}

func (r TickRegime) TickSize(price float64) float64 {
	if r == TickRegimeFine {
		return fineTickSize(price)
	}
	return TickSize(price)
}

// 観測された約定値から呼値テーブルを判定する純粋関数。粗い銘柄は定義上その刻みの倍数でしか約定しないので
// 「刻みに乗らない値が1本でもある」は Fine の決定的証拠になるが、逆は成り立たない(細かい銘柄でも全部が
// 倍数になりうる)。よって Fine は証拠があるときだけ返し、無ければ Coarse へ倒す — 誤りの向きを固定する
// 非対称で、Coarse を Fine と誤ると粗いグリッドに乗らない TP/SL を作って broker に弾かれる(実弾で守りが消える)。
func InferTickRegime(prices []float64) TickRegime {
	for _, p := range prices {
		if p <= 0 {
			continue // データ欠損。証拠にしない
		}
		if !isAlignedTo(p, TickSize(p)) {
			return TickRegimeFine
		}
	}
	return TickRegimeCoarse
}

// 証拠が「古い」と見なすクリーンな末尾の長さ(バー数)。
//
// 本物の細かい呼値の銘柄は**ほぼ毎日**粗いグリッドから外れた値を出す(実測: 7203 / 8306 は
// 四本値ベースで密度 0.90)。したがって 20 本連続で 1 つも外れないことは、その銘柄が
// 細かい呼値で**約定していない**ことの強い証拠になる(密度 0.9 なら偶然は 1e-20)。
// 逆に分割調整の痕跡は、権利落ち日を境に**ぱたりと止まって二度と現れない**。
// この非対称が両者を分ける唯一の観測可能な差。
const staleTickEvidenceBars = 20

// InferTickRegimeFromBars は時系列のバー列から呼値テーブルを判定する。
//
// `InferTickRegime` との違いは**証拠の鮮度を見る**こと。CSV は `ChainLinkSplits` 適用後の
// 値を持つので、分割の前のバーは raw/N になって粗いグリッドから外れる。素の
// `InferTickRegime` はこれを Fine の決定的証拠と読むが、調整値は約定値ではないので
// 前提が成り立たない(実測で fine 585 銘柄中 93 銘柄がこれだった)。
//
// 判定: 最後に外れたバーより後ろが staleTickEvidenceBars 本以上クリーンなら、その証拠は
// **過去の調整に由来する**と見て Coarse へ倒す。誤りの向きは `InferTickRegime` と同じく
// 粗い側(fail-safe)— Fine を Coarse と誤っても刻みが粗くなるだけだが、逆は格子外の
// TP/SL を作って broker に注文ごと拒否される。
func InferTickRegimeFromBars(bars []Candle) TickRegime {
	lastOff := -1
	for i, b := range bars {
		if InferTickRegime([]float64{b.Open, b.High, b.Low, b.Close}) == TickRegimeFine {
			lastOff = i
		}
	}
	if lastOff < 0 {
		return TickRegimeCoarse // 証拠なし
	}
	if len(bars)-1-lastOff >= staleTickEvidenceBars {
		return TickRegimeCoarse // 証拠が古い = 分割調整の痕跡
	}
	if splitAdjustmentExplains(bars, lastOff) {
		return TickRegimeCoarse // 継ぎ目が最近すぎて鮮度では落ちない分割調整
	}
	return TickRegimeFine
}

// 継ぎ目より後ろがこの本数以上クリーンなら「off-grid は継ぎ目より前だけ」と見なす。
// 鮮度(staleTickEvidenceBars)より遥かに短いのは、こちらは比を掛け戻す**直接の反証**を
// 併せて要求するため。本物の細かい呼値の値がすべて単一の分割比で粗いグリッドへ戻ることは
// 実質起こらない(実測 492 銘柄で 0 件)。
const splitArtifactCleanTailBars = 3

// splitAdjustmentExplains は「粗いグリッドから外れた値が、単一の単純分割比を掛け戻すと
// すべて粗いグリッドへ戻り、かつ継ぎ目より前だけに出ている」かを見る。
//
// `ChainLinkSplits` は分割前のバーを比で割るので、調整値は定義上この性質を持つ。
// 逆に本物の細かい呼値の約定値は元から粗いグリッドの外にあり、比を掛けても戻らない。
// 鮮度で落とせない**直近の分割**(例: 8011 / 9279 / 9900 = 4 営業日前)を
// これで落とす。
func splitAdjustmentExplains(bars []Candle, lastOff int) bool {
	if len(bars)-1-lastOff < splitArtifactCleanTailBars {
		return false // 継ぎ目が見えない = 調整だと言い切れない
	}
	var off []float64
	for _, b := range bars[:lastOff+1] {
		for _, p := range [4]float64{b.Open, b.High, b.Low, b.Close} {
			if p > 0 && !isAlignedTo(p, TickSize(p)) {
				off = append(off, p)
			}
		}
	}
	if len(off) == 0 {
		return false
	}
	for _, r := range simpleSplitRatios {
		all := true
		for _, p := range off {
			raw := p * r
			if !isAlignedTo(raw, TickSize(raw)) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}
