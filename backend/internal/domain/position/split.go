package position

import (
	"fmt"
	"math"
	"time"

	"stockbot/backend/internal/domain/market"
)

// SplitAdjusted は株式分割(併合)の権利落ちに合わせて建玉を**言い直す**。r は株数の倍率
// (1:5 分割なら 5、10:1 併合なら 0.1)で、day は権利落ち日(SplitAdjustedOn に記録する)。
//
// 🚨 なぜ要るか(paper の 6368): 建値・TP・SL は**分割前の円**で凍結してあるので、
// 言い直さないと権利落ちの寄りで SL を「割った」と読み、実際は含み益の建玉を見かけ −90 万円で
// 損切りする。
//
// 🛑 **経済的な中身は 1 円も変えない**: 株数を r 倍、円/株 の値(建値・TP/SL の価格と幅・
// ratchet・peak/trough・延長/早期利確の閾値)を 1/r にするので、建玉金額と各出口までの損益は
// 分割の前後で等しい。手数料・保有期限・arm 済みかどうかは分割と無関係なので触らない。
// これは config 凍結の例外ではなく**同じ凍結値を新しい単位で書き直す**だけ。
//
// 🚨 **TP/SL の価格は呼値の格子へ丸める**(`market.RoundToTickOf`・丸めの正本)。板の守りは
// この値をそのまま broker へ送る経路(RearmUnguarded / ArmProtectiveOrder)があり、
// 2,659 ÷ 5 = 531.8 円のような格子外の値段は立花が注文ごと拒否する = 建玉が裸で残る。
// 幅(円/株)と建値は丸めない(建値は VWAP と同じく格子外でよい)。
//
// 株数が整数にならない分割は error(端数は broker の権利処理次第で、推測で株数を作ると
// 台帳と broker の建玉がずれる)。呼び手は調整せず、人間に回すこと。
func SplitAdjusted(p Position, r float64, day time.Time) (Position, error) {
	if !(r > 0) || math.IsInf(r, 0) || r == 1 {
		return p, fmt.Errorf("分割調整: 株数の倍率 %v は不正", r)
	}
	q := float64(p.Quantity) * r
	qi := math.Round(q)
	if math.Abs(q-qi) > 1e-9 || qi < 1 {
		return p, fmt.Errorf("分割調整: %d 株 × %v = %v 株は整数にならない(端数の扱いは broker の権利処理次第)",
			p.Quantity, r, q)
	}
	out := p
	out.Quantity = int(qi)
	for _, v := range []*float64{
		&out.EntryPrice, &out.TakeProfitPrice, &out.StopLossPrice,
		&out.TakeProfitJPY, &out.StopLossJPY,
		&out.ExtensionUnrealizedJPY, &out.EarlyExitTargetJPY,
		&out.RatchetArmJPY, &out.RatchetGivebackJPY,
		&out.PeakUnrealizedJPY, &out.TroughUnrealizedJPY,
	} {
		*v /= r
	}
	for _, v := range []*float64{&out.TakeProfitPrice, &out.StopLossPrice} {
		if *v > 0 {
			*v = market.RoundToTickOf(p.Symbol, *v)
		}
	}
	f := p.SplitFactor
	if f <= 0 {
		f = 1
	}
	out.SplitFactor = f * r
	out.SplitAdjustedOn = day
	return out, nil
}
