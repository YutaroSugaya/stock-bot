package command

import (
	"context"
	"fmt"

	"stockbot/backend/internal/domain/order"
)

// activeOrderSource は「いま板に残っている注文」の照会口。**実 wire を叩く実装だけ**が
// この検査に意味を与える(立花は `GetActiveOrders` = 注文一覧照会)。
type activeOrderSource interface {
	GetActiveOrders(ctx context.Context, symbol string) ([]order.Order, error)
}

// verifyProtectiveOrder は建玉に対する守り(決済側の注文)が **broker の板に実在する**
// ことを確かめる。
//
// 🛑 なぜ `ResolveSettleLegs` では足りないか: 立花アダプタのそれは
// `t.settleLegs[bpID]`(**プロセス内 map**)を読むだけで broker に問い合わせない。
// `PlaceSettleOCO` が成功を返しさえすれば、注文が実際には板に無くても entry saga は
// 素通りする。それは「置いたと覚えている」であって「守られている」ではない。
// CLAUDE.md「TP/SL は必ず broker 側」を実際に満たしているかを見る唯一の検査。
//
// 🛑 照会が失敗したときは **拒否**(fail-close)。「確かめられなかった」を「大丈夫だった」
// と読むと、この検査は broker 障害時に自動で無効化される — 守りが一番怪しい場面で。
func verifyProtectiveOrder(ctx context.Context, src activeOrderSource, symbol string, closeSide order.Side, qty int) error {
	orders, err := src.GetActiveOrders(ctx, symbol)
	if err != nil {
		return fmt.Errorf("守りの実在確認ができない(注文一覧の照会に失敗): %w", err)
	}
	if !protectiveOrderIsResting(orders, symbol, closeSide, qty) {
		return fmt.Errorf("守りが板に無い: %s の %s %d株 に対する決済注文が注文一覧に存在しない(PlaceSettleOCO は成功を返したが実際には置かれていない)", symbol, closeSide, qty)
	}
	return nil
}

// protectiveOrderIsResting は決済側・同銘柄・**逆指値脚を持つ**・数量十分な注文が
// 1 本でもあるかを見る。
//
// 数量は **>= qty** を要求する: 建玉 100 株に対して 99 株ぶんしか守りが無ければ、
// 残り 1 株は裸。部分的な守りを「守りあり」と数えない。
//
// 🛑 **`HasStopLeg` を要求するのが本体**。決済側に注文があることは守りの証明では
// ない — 利確指値だけが板にある建玉は下方向に裸で、SL はどこにも存在しない。
// ここが数量だけを見ていたため、「✅ 守りあり」は「売り注文が板にある」としか
// 言っていなかった。CLAUDE.md「TP/SL は必ず
// broker 側」のうち、**SL の側**を実際に確かめているのはこの述語だけ。
func protectiveOrderIsResting(orders []order.Order, symbol string, closeSide order.Side, qty int) bool {
	for _, o := range orders {
		if o.Symbol != symbol || o.Side != closeSide || !o.HasStopLeg {
			continue
		}
		if o.Quantity >= qty {
			return true
		}
	}
	return false
}
