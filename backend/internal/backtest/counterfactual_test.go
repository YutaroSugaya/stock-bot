package backtest_test

import (
	"testing"
	"time"

	"stockbot/backend/internal/backtest"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

// 本番の CSV ローダは JST の日付をパースして **UTC で格納**する(csvload.go)。
// テストもその形に揃える — 瞬間比較なので location には依存しないが、その不変条件を
// 主張しておく。
func bar(d int, high, low, close float64) market.Candle {
	return market.Candle{
		Symbol: "7203", Interval: 24 * time.Hour,
		OpenTime: time.Date(2026, 8, d, 0, 0, 0, 0, clock.JST).UTC(),
		Open:     (high + low) / 2, High: high, Low: low, Close: close,
	}
}

func closedOn(d int) time.Time { return time.Date(2026, 8, d, 15, 0, 0, 0, clock.JST) }

func TestCounterfactual_BuySide(t *testing.T) {
	bars := []market.Candle{
		bar(14, 1010, 990, 1000), // 決済当日(既に実現済み → 見ない)
		bar(17, 1050, 995, 1040), // TP 1100 にも SL 950 にも届かない
		bar(18, 1120, 1010, 1100),
	}
	got := backtest.Counterfactual(backtest.CFInput{
		Side: order.SideBuy, TakeProfitPrice: 1100, StopLossPrice: 950,
		After: closedOn(14), Bars: bars,
	})
	if got.Outcome != backtest.CFTakeProfit {
		t.Fatalf("outcome = %q, want take_profit", got.Outcome)
	}
	if got.Days != 2 {
		t.Fatalf("Days = %d, want 2(決済日の翌バーを 1 と数える)", got.Days)
	}
}

func TestCounterfactual_StopFirst(t *testing.T) {
	bars := []market.Candle{bar(14, 1010, 990, 1000), bar(17, 1020, 940, 945)}
	got := backtest.Counterfactual(backtest.CFInput{
		Side: order.SideBuy, TakeProfitPrice: 1100, StopLossPrice: 950,
		After: closedOn(14), Bars: bars,
	})
	if got.Outcome != backtest.CFStopLoss || got.Days != 1 {
		t.Fatalf("got %+v, want stop_loss on day 1", got)
	}
}

// 🛑 同じ日足で両方に触れたら**順序は決まらない**。有利な方へ倒すと反実仮想が
// 勝ち探しになるので、必ず ambiguous として別に数える。
func TestCounterfactual_SameBarTouchesBothIsAmbiguous(t *testing.T) {
	bars := []market.Candle{bar(14, 1010, 990, 1000), bar(17, 1150, 900, 1000)}
	got := backtest.Counterfactual(backtest.CFInput{
		Side: order.SideBuy, TakeProfitPrice: 1100, StopLossPrice: 950,
		After: closedOn(14), Bars: bars,
	})
	if got.Outcome != backtest.CFAmbiguous {
		t.Fatalf("outcome = %q, want ambiguous", got.Outcome)
	}
}

// 日足が尽きたら未決着。**終値で「利確したこと」にしない** — 最終終値は別フィールドで
// 返し、集計側が mark-to-market として扱う。
func TestCounterfactual_RunsOutOfBars(t *testing.T) {
	bars := []market.Candle{bar(14, 1010, 990, 1000), bar(17, 1050, 1000, 1020)}
	got := backtest.Counterfactual(backtest.CFInput{
		Side: order.SideBuy, TakeProfitPrice: 1100, StopLossPrice: 950,
		After: closedOn(14), Bars: bars,
	})
	if got.Outcome != backtest.CFUnresolved || got.LastClose != 1020 {
		t.Fatalf("got %+v, want unresolved with last close 1020", got)
	}
	if got.Days != 0 {
		t.Fatalf("未決着に Days を入れない: %+v", got)
	}
}

// TP を持たない戦略(trail)は SL だけで判定する。0 を「即到達」と読まない。
func TestCounterfactual_NoTakeProfitLeg(t *testing.T) {
	bars := []market.Candle{bar(14, 1010, 990, 1000), bar(17, 5000, 4000, 4500)}
	got := backtest.Counterfactual(backtest.CFInput{
		Side: order.SideBuy, TakeProfitPrice: 0, StopLossPrice: 950,
		After: closedOn(14), Bars: bars,
	})
	if got.Outcome != backtest.CFUnresolved {
		t.Fatalf("outcome = %q — TP=0 を到達扱いにしてはいけない", got.Outcome)
	}
}

func TestCounterfactual_SellSideMirrors(t *testing.T) {
	bars := []market.Candle{bar(14, 1010, 990, 1000), bar(17, 1010, 890, 900)}
	got := backtest.Counterfactual(backtest.CFInput{
		Side: order.SideSell, TakeProfitPrice: 900, StopLossPrice: 1050,
		After: closedOn(14), Bars: bars,
	})
	if got.Outcome != backtest.CFTakeProfit {
		t.Fatalf("outcome = %q, want take_profit(売りは下がれば利確)", got.Outcome)
	}
}

// 🚨 決済後のバーが 1 本も無いのは**未決着ではない**(「最終終値」が存在しない)。
// 締め当日に走らせると全件がこれになるので、損益に混ぜてはいけない。
func TestCounterfactual_NoBarsAfterCloseIsNoData(t *testing.T) {
	got := backtest.Counterfactual(backtest.CFInput{
		Side: order.SideBuy, TakeProfitPrice: 1100, StopLossPrice: 950,
		After: closedOn(14), Bars: []market.Candle{bar(14, 1010, 990, 1000)},
	})
	if got.Outcome != backtest.CFNoData {
		t.Fatalf("outcome = %q, want no_data", got.Outcome)
	}
	if got.LastClose != 0 || got.BarsWalked != 0 {
		t.Fatalf("材料が無いのに値が入っている: %+v", got)
	}
}

// TP も SL も無い建玉(外部採用など)は必ず未決着。0 を「即到達」と読まない。
func TestCounterfactual_NoLegsAtAll(t *testing.T) {
	got := backtest.Counterfactual(backtest.CFInput{
		Side: order.SideBuy, After: closedOn(14),
		Bars: []market.Candle{bar(14, 1010, 990, 1000), bar(17, 5000, 100, 200)},
	})
	if got.Outcome != backtest.CFUnresolved {
		t.Fatalf("outcome = %q, want unresolved", got.Outcome)
	}
}

// 決着したバーの始値を返す(呼び手が窓開けの約定価格を決められるように)。
func TestCounterfactual_ReportsHitBarOpen(t *testing.T) {
	got := backtest.Counterfactual(backtest.CFInput{
		Side: order.SideBuy, TakeProfitPrice: 1100, StopLossPrice: 950,
		After: closedOn(14), Bars: []market.Candle{bar(14, 1010, 990, 1000), bar(17, 1200, 1180, 1190)},
	})
	if got.Outcome != backtest.CFTakeProfit || got.HitOpen != 1190 {
		t.Fatalf("got %+v — 到達バーの始値を返すこと", got)
	}
}

// 決済**より前**のバーは、バリアに触れていても見ない(建玉期間中の値動きは実現済み)。
func TestCounterfactual_IgnoresBarsBeforeClose(t *testing.T) {
	got := backtest.Counterfactual(backtest.CFInput{
		Side: order.SideBuy, TakeProfitPrice: 1100, StopLossPrice: 950,
		After: closedOn(14),
		Bars: []market.Candle{
			bar(12, 1200, 900, 1000), // 建玉期間中に両方に触れている
			bar(13, 1200, 900, 1000),
			bar(14, 1010, 990, 1000),
			bar(17, 1050, 1000, 1020),
		},
	})
	if got.Outcome != backtest.CFUnresolved || got.BarsWalked != 1 {
		t.Fatalf("got %+v — 決済後の 1 本だけ歩くこと", got)
	}
}
