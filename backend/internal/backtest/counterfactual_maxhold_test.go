package backtest

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

func bars(t *testing.T, start time.Time, ohlc ...[4]float64) []market.Candle {
	t.Helper()
	out := make([]market.Candle, 0, len(ohlc))
	for i, c := range ohlc {
		out = append(out, market.Candle{
			OpenTime: start.AddDate(0, 0, i+1),
			Open:     c[0], High: c[1], Low: c[2], Close: c[3],
		})
	}
	return out
}

// 🚨 **凍結された MaxHold より先は歩いてはいけない。**
//
// `manual` / `forced_flat`(サイクルの締めで人間が手仕舞いした建玉)の反実仮想が
// 答えるべき問いは「**手仕舞いしなければどうなっていたか**」であって、
// 「MaxHold も無かったらどうなっていたか」ではない。キャップを無視して歩くと、
// 期限の後に来た TP/SL を拾って**存在しない出口**を数える。
//
// config 系の戦略にも期限が付いているので、実質全件に効く。
func TestCounterfactual_StopsAtTheFrozenMaxHold(t *testing.T) {
	closed := time.Date(2026, 8, 21, 15, 0, 0, 0, clock.JST)
	// 建値 1000 / TP 1100 / SL 900。TP に当たるのは 5 本目。
	b := bars(t, closed,
		[4]float64{1000, 1010, 990, 1000},
		[4]float64{1000, 1020, 995, 1010},
		[4]float64{1010, 1030, 1000, 1020},
		[4]float64{1020, 1040, 1010, 1030},
		[4]float64{1030, 1150, 1020, 1140}, // ここで TP
	)
	in := CFInput{Side: order.SideBuy, TakeProfitPrice: 1100, StopLossPrice: 900, After: closed, Bars: b}

	t.Run("期限が無ければ TP に届く", func(t *testing.T) {
		if got := Counterfactual(in); got.Outcome != CFTakeProfit {
			t.Fatalf("outcome = %s, want take_profit", got.Outcome)
		}
	})

	t.Run("期限が 3 本目なら期限切れで終わる", func(t *testing.T) {
		capped := in
		capped.Until = b[2].OpenTime
		got := Counterfactual(capped)
		if got.Outcome != CFMaxHold {
			t.Fatalf("outcome = %s, want max_hold — 期限の後の TP を拾っている", got.Outcome)
		}
		// 期限のバーの終値で決着する(そこで成行返済されるため)。
		if got.LastClose != 1020 {
			t.Fatalf("LastClose = %v, want 1020(期限バーの終値)", got.LastClose)
		}
		if got.Days != 3 {
			t.Fatalf("Days = %d, want 3", got.Days)
		}
	})

	// 期限より前に当たったバリアはそのまま採る(キャップは「それ以上持たない」だけ)。
	t.Run("期限より前のバリアは有効", func(t *testing.T) {
		early := in
		early.StopLossPrice = 995 // 1 本目の安値 990 で当たる
		early.Until = b[3].OpenTime
		if got := Counterfactual(early); got.Outcome != CFStopLoss {
			t.Fatalf("outcome = %s, want stop_loss", got.Outcome)
		}
	})

	// 期限が日足の範囲より先 = まだ来ていない。従来どおり未決着。
	t.Run("期限がデータより先なら未決着", func(t *testing.T) {
		far := in
		far.TakeProfitPrice = 9999
		far.StopLossPrice = 1
		far.Until = closed.AddDate(0, 1, 0)
		if got := Counterfactual(far); got.Outcome != CFUnresolved {
			t.Fatalf("outcome = %s, want unresolved", got.Outcome)
		}
	})

	// Until が zero = キャップ無し(max_hold 決済の反実仮想。従来どおり)。
	t.Run("Until が zero なら従来どおり", func(t *testing.T) {
		if got := Counterfactual(in); got.Outcome != CFTakeProfit {
			t.Fatalf("outcome = %s", got.Outcome)
		}
	})
}

// 🚨 **期限の比較は日付で行う。**
//
// 日足の OpenTime は JST 0 時、`Until` は建玉時刻 + 保有分なので必ず場中の時刻になる。
// 時刻で比べると期限当日のバーが期限扱いされず、**常に 1 営業日ぶん余計に歩く**。
// 直そうとした「存在しない出口を数える」が
// 1 バーぶん残っていた。
func TestCounterfactual_CapComparesByDayNotInstant(t *testing.T) {
	closed := time.Date(2026, 8, 21, 15, 0, 0, 0, clock.JST)
	b := bars(t, closed,
		[4]float64{1000, 1010, 990, 1000},  // 8/22
		[4]float64{1000, 1020, 995, 1010},  // 8/23
		[4]float64{1010, 1200, 1000, 1190}, // 8/24 ← ここで TP 1100(期限の翌バー)
	)
	// 期限は 8/23 の **12:30**(場中の時刻)。日足の 8/23 バーは OpenTime 0 時。
	until := time.Date(2026, 8, 23, 12, 30, 0, 0, clock.JST)
	got := Counterfactual(CFInput{
		Side: order.SideBuy, TakeProfitPrice: 1100, StopLossPrice: 900,
		After: closed, Until: until, Bars: b,
	})
	if got.Outcome != CFMaxHold {
		t.Fatalf("outcome = %s, want max_hold — 期限の翌バーの TP を拾っている(1 営業日ぶん余計に歩いた)", got.Outcome)
	}
	if got.LastClose != 1010 {
		t.Fatalf("LastClose = %v, want 1010(8/23 の終値)", got.LastClose)
	}
}

// 🚨 **期限が土日祝に落ちてその日のバーが無いとき**、翌営業日のバリアを拾ってはいけない。
//
// 1 巡目の修正は「期限**当日**のバー」だけを見ていたので、期限が休場日だと atCap が
// 一度も立たず、翌営業日の高値が TP を拾って**存在しない利確益**を計上していた。
// 実機は期限を過ぎた最初のティック(≒寄り値)で
// 成行返済する。
func TestCounterfactual_CapOnAHolidayClosesAtTheNextOpen(t *testing.T) {
	closed := time.Date(2026, 8, 21, 15, 0, 0, 0, clock.JST) // 金曜に手仕舞い
	// 次のバーは月曜(8/24)。土曜のバーは存在しない。
	b := []market.Candle{{
		OpenTime: time.Date(2026, 8, 24, 0, 0, 0, 0, clock.JST),
		Open:     1010, High: 1200, Low: 1000, Close: 1190,
	}}
	// 期限は **8/22(土)10:00** = 休場日。
	until := time.Date(2026, 8, 22, 10, 0, 0, 0, clock.JST)
	got := Counterfactual(CFInput{
		Side: order.SideBuy, TakeProfitPrice: 1100, StopLossPrice: 900,
		After: closed, Until: until, Bars: b,
	})
	if got.Outcome != CFMaxHold {
		t.Fatalf("outcome = %s, want max_hold — 期限を過ぎたバーの高値で「存在しない利確」を計上している", got.Outcome)
	}
	// 期限を過ぎた最初のティック = 寄り値で返済(終値ではない — 一晩のギャップと
	// その日 1 日ぶんの値動きは、もう存在しない建玉のもの)。
	if got.LastClose != 1010 {
		t.Fatalf("約定値 = %v, want 1010(寄り値)", got.LastClose)
	}
}

// 期限当日のバーがあるときは、日中のバリアが先(成行返済は引けなので実機でもそうなる)。
func TestCounterfactual_BarrierWinsOnTheDeadlineDayItself(t *testing.T) {
	closed := time.Date(2026, 8, 21, 15, 0, 0, 0, clock.JST)
	b := []market.Candle{{
		OpenTime: time.Date(2026, 8, 24, 0, 0, 0, 0, clock.JST),
		Open:     1010, High: 1200, Low: 1000, Close: 1190,
	}}
	until := time.Date(2026, 8, 24, 12, 30, 0, 0, clock.JST) // 期限当日
	got := Counterfactual(CFInput{
		Side: order.SideBuy, TakeProfitPrice: 1100, StopLossPrice: 900,
		After: closed, Until: until, Bars: b,
	})
	if got.Outcome != CFTakeProfit {
		t.Fatalf("outcome = %s, want take_profit(期限当日の日中に当たったバリアは実際にも約定していた)", got.Outcome)
	}
}
