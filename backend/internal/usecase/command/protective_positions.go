package command

// Shared helpers of the protective-order commands (Arm / Rearm / Replace / Reprice):
// which positions carry a broker-side guard, and which resting order belongs to which position.

import (
	"context"
	"fmt"
	"time"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// ownsProtectiveOrder は「その注文が bot の建玉の守りか」。判定は**向きだけ** ——
// 守りは建玉の反対側に出る。
//
// 🚨 **数量を条件に入れない**。板の決済注文数量が台帳の
// 建玉数量と一致しない状態は本番で実際に起きる(部分約定・補償の残骸)。数量一致を
// 必要条件にすると、そのとき**実弾の守りを「他人の注文」と判定して延長せず**、
// 9 営業日目に裸になる。
//
// 非対称で考える: 延ばして困る注文は無い(期日以外は "*" = 変更なし)。延ばさないと
// 建玉が裸になる。**分からないなら延ばす側**に倒す。
//
// 向きの条件は残す — 建玉と同じ側の注文(= 人間の新規注文など)は守りではないので、
// これで「同じ銘柄に人間が置いた注文」を巻き込む主な経路は消える。
func ownsProtectiveOrder(held []position.Position, o port.ProtectiveOrderInfo) bool {
	for _, p := range held {
		if o.Side == p.Side.Opposite() {
			return true
		}
	}
	return false
}

// matchProtectivePosition は守りの注文 1 本に対応する**建玉を 1 つに確定**する。
//
// 🚨 `ownsProtectiveOrder` より厳しい(side だけでなく数量も見る)。使い分けの理由:
//
//   - **訂正(renew)** は期日しか変えないので、どの建玉の守りかを取り違えても
//     壊れるものが無い。所有権(= 人間の注文に触らない)だけ確かめれば足りる。
//   - **置き直し(replace)** は注文を組み直すので、**建玉固有の値**(口座区分・
//     建玉 ID)が要る。取り違えたまま取消を撃つと再発注が組めず、建玉が裸で残る。
//
// 曖昧(候補 0 本 / 2 本以上)なら false。呼び手は**取り消さずに**やめること。
// この区別が無いまま置き直しを撃つと、実弾の建玉を裸にする。
func matchProtectivePosition(held []position.Position, o port.ProtectiveOrderInfo) (position.Position, bool) {
	var found position.Position
	n := 0
	for _, p := range held {
		if o.Side != p.Side.Opposite() || p.Quantity != o.Quantity {
			continue
		}
		found, n = p, n+1
	}
	if n != 1 {
		return position.Position{}, false
	}
	return found, true
}

// needsRenewal は「この守りは今すぐ延ばすべきか」。
//
// 🛑 zero(= 当日限り / 期日が読めない)は **true**。「無期限」と読むと、引けで消える
// 守りを一番安全な状態だと誤判定する — 多日保有では最も危険な状態そのもの。
func needsRenewal(expireOn, deadline time.Time) bool {
	if expireOn.IsZero() {
		return true
	}
	if deadline.IsZero() { // カレンダー切れ。判定できないので延長側へ倒す(fail-close)
		return true
	}
	return !expireOn.After(deadline)
}

func expiryLabel(t time.Time) string {
	if t.IsZero() {
		return "当日限り"
	}
	return t.Format("2006-01-02")
}

// multidayGuardedPositions は銘柄ごとに **bot の multiday 建玉**を返す。
//
// 🛑 「bot が守りを置いた建玉か」の判定を 2 通り持たないため、置き直し
// (ReplaceProtectiveOrder)と値段の変更(RepriceProtectiveOrder)で共有する。
// 片方だけ条件が変わると、片方は触るのにもう片方は触らない建玉が生まれる。
//
//   - multiday 以外は除く(intraday は 14:50 に強制フラット化されるので当日限りが正しい)
//   - external(人間の建玉)は除く — bot は守りを置いていない
//   - CLOSING は除く。守りを取り消して成行返済を板に出した後なので「逆指値が無い」のは
//     **正常な状態**。含めると 30 分ごとに事実と違う 🚨 が鳴り、狼少年化して
//     本物の期日切れを見落とす。
func multidayGuardedPositions(ctx context.Context, pr port.PositionRepository) (map[string][]position.Position, error) {
	return multidayPositions(ctx, pr, false)
}

// multidayPositions は多日・非 external の bot 建玉を銘柄ごとに返す。
//
// includeClosing=false(既定)は **値段や期日を触る経路**のためのもの。決済中の建玉に
// 訂正や置き直しを走らせない。
//
// includeClosing=true は **arm(守りの新規設置)専用**。守りを cancel した後に決済が
// 板へ載らないと建玉は CLOSING かつ裸になるが、そこを除外していたため
// 「bot が自力で裸にした状態を bot は直せない」状態になる(人間が証券アプリで
// 直すしかなくなる構造的な理由)。
// 🛑 arm 側は **板に決済側の注文が無いこと**を別途確かめてから置く — 決済中の建玉に
// 返済注文を重ねると実質の新規売りになりうる、という元の懸念は正しい。
func multidayPositions(ctx context.Context, pr port.PositionRepository, includeClosing bool) (map[string][]position.Position, error) {
	positions, err := pr.ListOpenAllSymbols(ctx)
	if err != nil {
		return nil, fmt.Errorf("建玉一覧が読めない: %w", err)
	}
	guarded := map[string][]position.Position{}
	for _, p := range positions {
		if p.HoldingMode != order.HoldingMultiday {
			continue
		}
		if p.Source == position.SourceExternal {
			continue
		}
		if p.Status == position.StatusClosing && !includeClosing {
			continue
		}
		guarded[p.Symbol] = append(guarded[p.Symbol], p)
	}
	return guarded, nil
}
