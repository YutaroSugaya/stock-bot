package position

import (
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

// 円/株の幅(建値からの距離)を絶対 broker 価格へ変換する。幅 0 は価格 0(= その脚は無い)。
// 呼値が登場するのはここだけ — 発注できる値段に乗せる最後の一手でだけグリッドに丸める。
func TPSLPricesFromJPY(symbol string, side order.Side, entry, tpJPY, slJPY float64) (tp, sl float64) {
	switch side {
	case order.SideBuy:
		if tpJPY > 0 {
			tp = market.RoundToTickOf(symbol, entry+tpJPY)
		}
		if slJPY > 0 {
			sl = market.RoundToTickOf(symbol, entry-slJPY)
		}
	case order.SideSell:
		if tpJPY > 0 {
			tp = market.RoundToTickOf(symbol, entry-tpJPY)
		}
		if slJPY > 0 {
			sl = market.RoundToTickOf(symbol, entry+slJPY)
		}
	}
	return tp, sl
}
