package command

import (
	"context"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// 建玉が broker から消えたときに「何が起きたのか」を実額で確定するための 2 本。
//
// 🚨 なぜ要るか(8604):
// 板の逆指値(SL 1,596)が約定して建玉が消えた。bot はそれを
//   ①`closeOne` が「返済が拒否された = 建玉は裸」と読んで **live を緊急停止**し、
//   ②`RearmUnguarded` が 15 分ごとに **決済済みの建玉へ守りを置こうとして**拒否され、
//   ③`Reconcile` が 92 分後に **時価 1,596.5** を `reconcile_cold_close` として記帳した。
// 3 つとも同じ読み違い —— **「建玉が無い」と「守りが無い」は正反対**である。
// 守るものが無い建玉に守りは置けないし、それは裸でもない。起きたのは戦略の出口
// (損切り)で、値段も手数料も約定照会に載っている。

// brokerPositionLister は建玉照会だけを要る面で切ったもの。**唯一の権威**で、
// 拒否メッセージの文面では代用しない —— 同じ文面は建玉の指定違いでも出るので、
// 生きている建玉を「決済済み」と誤読すると守りごと取り下げることになる。
type brokerPositionLister interface {
	GetPositions(ctx context.Context) ([]port.BrokerPosition, error)
}

// positionGoneAtBroker は「broker はもうこの建玉を持っていないか」。
//
// 戻り値の checked は **確かめられたか**。照会が落ちた回は (false, false) を返し、
// 呼び手は従来どおり安全側(裸として扱う / 守りを置きに行く)へ倒す。
// 「確かめられなかった」を「決済済み」と読むと、障害のたびに裸の建玉が静かに
// 見逃される。
func positionGoneAtBroker(ctx context.Context, brk brokerPositionLister, p position.Position) (gone, checked bool) {
	if brk == nil {
		return false, false
	}
	held, err := brk.GetPositions(ctx)
	if err != nil {
		return false, false
	}
	for _, bp := range held {
		// 🛑 **銘柄が残っていれば「消えていない」と読む。**建玉 ID で絞るほうが厳密に
		// 見えるが、立花の建玉 ID は銘柄ごとの合成値(`shinyo:<code>`)で、broker に
		// よっては建玉ごとに別 ID を振る。ID で絞ると「同じ銘柄の別建玉が残っている」
		// 状況を「消えた」と読みうる —— 誤る向きは必ず**まだ在るほう**に倒しておく。
		if bp.Symbol == p.Symbol {
			return false, true
		}
	}
	return true, true
}

// boardSettleFill は **建玉が消えた原因になった決済側の実約定**を約定照会から拾う。
// 戻りは (約定値, 決済手数料, 決済理由, 採れたか)。
//
// 🛑 **数量が建玉とぴったり一致したときだけ採る。** 約定照会は「どの建玉の決済か」を
// 保証しない(注文 id しか無い)ので、同じ銘柄に人間の売買が混ざった日に他人の値段で
// 台帳を締めうる。合わなければ採らず、呼び手は従来の近似(時価の cold close)へ倒す。
// 確かめられない値は書かない —— 幽霊決済の禁止と同じ極性。
func boardSettleFill(ctx context.Context, brk port.Broker, p position.Position) (price, fee float64, reason string, ok bool) {
	if brk == nil || p.Quantity <= 0 {
		return 0, 0, "", false
	}
	execs, err := brk.GetExecutions(ctx, 0)
	if err != nil {
		return 0, 0, "", false
	}
	closeSide := p.Side.Opposite()
	var qty int
	var notional float64
	for _, e := range execs {
		if e.Symbol != p.Symbol || e.Side != closeSide || e.Quantity <= 0 || e.Price <= 0 {
			continue
		}
		qty += e.Quantity
		notional += e.Price * float64(e.Quantity)
		fee += e.FeeJPY
	}
	if qty != p.Quantity {
		return 0, 0, "", false
	}
	price = notional / float64(qty) // 部分約定は数量加重平均(1 本なら約定値そのもの)
	return price, fee, boardFillReason(p, price), true
}

// boardFillReason は決済の実約定が **凍結した守りのどちら側で起きたか**で出口を決める。
//
// 🛑 **側に依らない形で書く。**SELL 建玉では SL が上・TP が下で大小が逆になる
// (`RatchetCloseReason` を gross の符号で判定したのと同じ理由)。
// 🛑 **SL を先に見る。**ギャップで両方を飛び越えた約定は損切りであって利確ではない。
// 🛑 **凍結値が 0 の脚は判定に使わない。**多日建玉の板は stop-only で
// TP 脚が載っていないので、値段だけで利確と読むと人間が締めた決済を戦略の利確として
// 台帳に書くことになる。
//
// 🛑 **どちらの脚でもない約定は `broker_close` 止まりで、意味づけを推測しない。**
// 板の値段は凍結値と違いうる(人間がアプリで締める / トレールで切り上がる)ので、
// 「凍結 SL より上で決済された = 利確」と機械的に書くことはできない。
// **建値より上へ切り上がっていた逆指値**が発動した 1 本を `take_profit` と読むかは
// **人間が台帳で下す判断**であって、ここが勝手に決めると、その判断の記録ごと消える。
func boardFillReason(p position.Position, price float64) string {
	sl, tp := p.StopLossPrice, p.TakeProfitPrice
	if p.Side == order.SideSell {
		switch {
		case sl > 0 && price >= sl:
			return port.CloseReasonStopLoss
		case tp > 0 && price <= tp:
			return port.CloseReasonTakeProfit
		}
		return port.CloseReasonBrokerClose
	}
	switch {
	case sl > 0 && price <= sl:
		return port.CloseReasonStopLoss
	case tp > 0 && price >= tp:
		return port.CloseReasonTakeProfit
	}
	return port.CloseReasonBrokerClose
}
