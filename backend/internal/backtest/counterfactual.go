package backtest

import (
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

// 反実仮想: **時間で打ち切った建玉が、そのまま持っていたら
// TP と SL のどちらに当たっていたか**を、決済後の日足から事後に計算する。
//
// これが無いとキャップ(MaxHold)や締めの手仕舞いの是非は**原理的に判定できない** —
// 閉じた後の道は観測できないため。純関数なので実行経路からは独立で、決済規則にも
// 一切触れない(計測器)。
//
// 🛑 **これは「もし持っていたら」の推定であって実現損益ではない。** 台帳の net と
// 混ぜない。同じ日に TP と SL の両方へ触れたバーは**どちらが先か分からない**ので
// ambiguous として別に数える(有利な方に倒すと反実仮想が勝ち探しになる)。

type CFOutcome string

const (
	CFTakeProfit CFOutcome = "take_profit"
	CFStopLoss   CFOutcome = "stop_loss"
	CFAmbiguous  CFOutcome = "ambiguous"  // 同一バーで両方に触れた(日足では順序が決まらない)
	CFUnresolved CFOutcome = "unresolved" // 日足が尽きるまでどちらにも当たらなかった
	// CFNoData = 決済後の日足が **1 本も無い**。未決着とは別物 — 「最終終値で評価」が
	// できないので損益に混ぜてはいけない。締め当日に走らせると(その日の日足はまだ
	// 翌朝の fetch-daily 待ちなので)全件がこれになる。
	CFNoData CFOutcome = "no_data"
	// CFMaxHold = **凍結された MaxHold の期限に達した**。バリアには当たっていないが、
	// その建玉は期限で成行返済されていたので「未決着」ではなく決着した出口。
	// 🛑 これを CFUnresolved と混ぜると、期限の後に来た TP/SL を拾って**存在しない
	// 出口**を数えることになる。
	CFMaxHold CFOutcome = "max_hold"
)

type CFInput struct {
	Side            order.Side
	TakeProfitPrice float64 // 0 = TP を持たない戦略(trail 等)。SL だけで判定する
	StopLossPrice   float64 // 0 = SL 無し(外部採用建玉など)
	// After は決済した日。**その翌バーから**歩く(決済当日の値動きは既に実現済み)。
	After time.Time
	// Until は**凍結された MaxHold の期限**(zero = キャップ無し)。
	//
	// 🚨 `manual` / `forced_flat` の反実仮想が答えるべきは「**手仕舞いしなければ
	// どうなっていたか**」であって「MaxHold も無かったら」ではない。ここを渡さないと
	// 期限の後に来たバリアを拾い、存在しない出口を数える。逆に `max_hold` 決済の
	// 反実仮想(= キャップが無ければどうだったか)では **zero を渡す**のが正しい。
	Until time.Time
	Bars  []market.Candle // 日足・古い順
}

type CFResult struct {
	Outcome CFOutcome
	HitDate time.Time
	// Days は決済日から何本目の日足で決着したか(1 = 翌営業日)。未決着は 0。
	// **暦の営業日ではなく CSV のバー本数** — 売買停止で穴が空いた銘柄では過小になる。
	Days int
	// LastClose は未決着のときの最終終値(mark-to-market の材料)。決着時は 0。
	LastClose float64
	// BarsWalked は決済後に歩いた日足の本数。0 = 材料が無い(CFNoData)。
	BarsWalked int
	// HitOpen は決着したバーの**始値**。窓を開けてバリアを飛び越えた日は、実際の約定は
	// バリア価格ではなく寄り値になる(逆指値は不利側・TP 指値は有利側に)。呼び手が
	// バリアと突き合わせて不利/有利の側を採る。
	HitOpen float64
}

// Counterfactual walks the bars after the close and reports which barrier the
// position would have reached first.
func Counterfactual(in CFInput) CFResult {
	n := 0
	var lastClose float64
	for _, b := range in.Bars {
		if !b.OpenTime.After(in.After) {
			continue
		}
		n++
		lastClose = b.Close
		// 期限との関係を **3 つに分ける**。
		//
		//   past = 期限を**過ぎた**日のバー … その建玉はもう存在しない。バリアを
		//          見てはいけない。実機は期限を過ぎた最初のティックで成行返済するので
		//          **寄り値**が約定値。
		//   at   = 期限**当日**のバー … 日中のバリアは実際にも当たっていた(成行返済は
		//          引け)。バリアを先に見て、当たらなければ**終値**で返済。
		//
		// 🚨 比較は**日付**で行う。日足の OpenTime は JST 0 時、`Until` は建玉時刻 +
		// 保有分なので必ず場中の時刻になり、時刻で比べると期限当日のバーが期限扱いに
		// ならない。さらに **期限が土日祝に落ちるとその日のバーが存在しない**ので、
		// 「期限当日」だけを見ていると翌営業日のバリアを拾ってしまう(1 巡目の修正で
		// 残っていた穴)。
		var pastCap, atCap bool
		if !in.Until.IsZero() {
			day, cap := barDay(b.OpenTime), barDay(in.Until)
			pastCap = day.After(cap)
			atCap = day.Equal(cap)
		}
		if pastCap {
			// 期限を過ぎた最初のバー。**バリアを見ない** — その建玉はもう無い。
			return CFResult{Outcome: CFMaxHold, HitDate: b.OpenTime, Days: n, BarsWalked: n,
				LastClose: b.Open, HitOpen: b.Open}
		}
		tp := in.TakeProfitPrice > 0 && reachedTP(in.Side, b, in.TakeProfitPrice)
		sl := in.StopLossPrice > 0 && reachedSL(in.Side, b, in.StopLossPrice)
		switch {
		case tp && sl:
			return CFResult{Outcome: CFAmbiguous, HitDate: b.OpenTime, Days: n, BarsWalked: n, HitOpen: b.Open}
		case tp:
			return CFResult{Outcome: CFTakeProfit, HitDate: b.OpenTime, Days: n, BarsWalked: n, HitOpen: b.Open}
		case sl:
			return CFResult{Outcome: CFStopLoss, HitDate: b.OpenTime, Days: n, BarsWalked: n, HitOpen: b.Open}
		case atCap:
			// 期限に達した。バリアに当たっていないので**そのバーの終値で成行返済**。
			return CFResult{Outcome: CFMaxHold, HitDate: b.OpenTime, Days: n, BarsWalked: n,
				LastClose: b.Close, HitOpen: b.Open}
		}
	}
	if n == 0 {
		// 材料が 1 本も無い。**「最終終値で評価」ではない** — 未決着と混ぜると、
		// 締め当日に走らせたときの「全件 0 円」が集計に足し込まれる。
		return CFResult{Outcome: CFNoData}
	}
	return CFResult{Outcome: CFUnresolved, LastClose: lastClose, BarsWalked: n}
}

// 買いは高値が TP に届けば利確、安値が SL を割れば損切。売りは逆。
func reachedTP(side order.Side, b market.Candle, tp float64) bool {
	if side == order.SideSell {
		return b.Low <= tp
	}
	return b.High >= tp
}

func reachedSL(side order.Side, b market.Candle, sl float64) bool {
	if side == order.SideSell {
		return b.High >= sl
	}
	return b.Low <= sl
}

// barDay は venue の暦日へ丸める。日足の OpenTime(JST 0 時)と、建玉時刻から
// 算出した期限(場中の時刻)を**同じ土俵**で比べるために要る。
func barDay(t time.Time) time.Time {
	x := t.In(clock.JST)
	return time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, clock.JST)
}
