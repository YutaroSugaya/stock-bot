package position

import "stockbot/backend/internal/domain/order"

// max_hold 延長の 1 回あたりの上限は**保有区分ごと**に決める。目的は「桁を間違えた延長を
// 弾く」ことだけで、幾何を守っているのは broker 側の逆指値であってこの数字ではない。
//
//   - intraday: 12時間。引けで強制フラット化されるのでそれ以上は意味を持たない。
//   - multiday: 30日。720分に縛ると 1 日延ばすのに 2 回・10日で 20 回叩くことになり
//     運用として成立しない。
//
// 🛑 **無制限にはしない。** broker 側の守り(`sOrderExpireDay`)は有限で、そちらを
// 越えて max_hold を伸ばすと「bot 側の出口だけが生きていて SL がどこにも無い」
// 建玉になる。上限を残すのは、その関係を人間が一度の入力で壊しにくくするため。
const (
	extendMaxMinutesIntraday = 720
	extendMaxMinutesMultiday = 30 * 24 * 60
)

// MaxExtendMinutes は保有区分ごとの 1 回あたり上限。延長の実行(command)と延長候補の
// 列挙(query)が同じ数字を読むためにここに置く(二重に書くと片方だけ動いてズレる)。
func MaxExtendMinutes(mode order.HoldingMode) int {
	if mode == order.HoldingMultiday {
		return extendMaxMinutesMultiday
	}
	return extendMaxMinutesIntraday
}
