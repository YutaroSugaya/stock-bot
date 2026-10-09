package strategy

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/ta"
)

// v2 戦略を固定する。v1 との差は入口の1点だけ・出口は v1 と同一。

// v1 は条件が継続する限り枠が空くたび入り直す。v2 は偽→真に転じたバーだけ取る。
func TestAbsMomentumV2_EntersOnlyOnFreshCross(t *testing.T) {
	// 200日線も 126日前も下回った状態から、最終バーで初めて両方を上回る系列。
	d := absV2FreshCross()
	v2 := AbsMomentumV2{}.Evaluate(candIn(config.StrategyAbsMomentumV2, d, 1))
	if !v2.IsEntry() || v2.Side != order.SideBuy {
		t.Fatalf("転じたバーでは入ること: decision=%q reason=%q", v2.Decision, v2.Reason)
	}
	// v1 も同じバーでは入る(v2 は v1 の部分集合であることの確認)。
	v1 := AbsMomentum{}.Evaluate(candIn(config.StrategyAbsMomentum, d, 1))
	if !v1.IsEntry() {
		t.Fatalf("前提: v1 も同じバーで入るはず: %q", v1.Reason)
	}
}

// 条件が「継続している」だけのバーでは v2 は入らない。v1 は入る — ここが唯一の差。
func TestAbsMomentumV2_SkipsWhenConditionMerelyPersists(t *testing.T) {
	d := steadyUp(absTrendSMA + 2) // 終始 200日線超・126日前超 = 状態が継続
	v1 := AbsMomentum{}.Evaluate(candIn(config.StrategyAbsMomentum, d, 1))
	if !v1.IsEntry() {
		t.Fatalf("前提: v1 は継続状態でも入る(これが直したい欠陥): %q", v1.Reason)
	}
	v2 := AbsMomentumV2{}.Evaluate(candIn(config.StrategyAbsMomentumV2, d, 1))
	if v2.IsEntry() {
		t.Fatal("v2 は継続状態では入らないこと(状態→事象の修正)")
	}
	if v2.Reason != "not_fresh_cross" {
		t.Fatalf("reason = %q, want not_fresh_cross", v2.Reason)
	}
}

// 出口は v1 と同一(変更は入口の1点だけ)。
func TestAbsMomentumV2_ExitGeometryUnchanged(t *testing.T) {
	d := absV2FreshCross()
	v2 := AbsMomentumV2{}.Evaluate(candIn(config.StrategyAbsMomentumV2, d, 1))
	if !v2.IsEntry() {
		t.Fatalf("前提: ENTER: %q", v2.Reason)
	}
	atr := ta.ATR(d, atrExitPeriod)
	if v2.TakeProfitJPY != atrExitTPMult*atr || v2.StopLossJPY != atrExitSLMult*atr {
		t.Fatalf("出口は v1 と同一のはず: TP=%v SL=%v (atr=%v)", v2.TakeProfitJPY, v2.StopLossJPY, atr)
	}
}

// v1 は圧縮の有無を問わず拡大に乗る。v2 はブレイク前日時点で ATR(14) < ATR(50) の場合に限る。
func TestATRBreakoutV2_RequiresPriorVolatilityCompression(t *testing.T) {
	// 圧縮あり: 長期は荒れていたが直近は凪 → 最終バーで拡大
	comp := atrV2Compressed()
	v2 := ATRBreakoutV2{}.Evaluate(candIn(config.StrategyATRBreakoutV2, comp, 1))
	if !v2.IsEntry() || v2.Side != order.SideBuy {
		t.Fatalf("圧縮後の拡大は入ること: decision=%q reason=%q", v2.Decision, v2.Reason)
	}

	// 圧縮なし: 直近がむしろ荒れている(ATR14 >= ATR50)→ v2 は見送り、v1 は入る
	exp := atrV2NotCompressed()
	v1 := ATRBreakout{}.Evaluate(candIn(config.StrategyATRBreakout, exp, 1))
	if !v1.IsEntry() {
		t.Fatalf("前提: v1 は圧縮を問わず入る(これが直したい欠陥): %q", v1.Reason)
	}
	got := ATRBreakoutV2{}.Evaluate(candIn(config.StrategyATRBreakoutV2, exp, 1))
	if got.IsEntry() {
		t.Fatal("圧縮していない拡大は v2 では見送ること")
	}
	if got.Reason != "no_prior_compression" {
		t.Fatalf("reason = %q, want no_prior_compression", got.Reason)
	}
}

// v1 はブレイク2日目以降も成立する。v2 は前バーがチャネル外でないことを要求する。
func TestDonchianBreakoutV2_OnlyFirstDayOfBreakout(t *testing.T) {
	first := dbV2FirstDay()
	v2 := DonchianBreakoutV2{}.Evaluate(candIn(config.StrategyDonchianBreakoutV2, first, 1))
	if !v2.IsEntry() || v2.Side != order.SideBuy {
		t.Fatalf("ブレイク初日は入ること: decision=%q reason=%q", v2.Decision, v2.Reason)
	}

	second := dbV2SecondDay()
	v1 := DonchianBreakout{}.Evaluate(candIn(config.StrategyDonchianBreakout, second, 1))
	if !v1.IsEntry() {
		t.Fatalf("前提: v1 はブレイク2日目でも入る(これが直したい欠陥): %q", v1.Reason)
	}
	got := DonchianBreakoutV2{}.Evaluate(candIn(config.StrategyDonchianBreakoutV2, second, 1))
	if got.IsEntry() {
		t.Fatal("ブレイク2日目は v2 では見送ること")
	}
	if got.Reason != "not_first_breakout" {
		t.Fatalf("reason = %q, want not_first_breakout", got.Reason)
	}
}

// v1 と v2 は別の Name を名乗ること(台帳・forward-report -strategy の分計単位)。
func TestV2StrategiesHaveDistinctNames(t *testing.T) {
	pairs := [][2]config.StrategyName{
		{AbsMomentum{}.Name(), AbsMomentumV2{}.Name()},
		{ATRBreakout{}.Name(), ATRBreakoutV2{}.Name()},
		{DonchianBreakout{}.Name(), DonchianBreakoutV2{}.Name()},
	}
	for _, p := range pairs {
		if p[0] == p[1] {
			t.Fatalf("v1/v2 が同名 (%s) — 分計できない", p[0])
		}
		if p[1] != p[0]+"_v2" {
			t.Fatalf("v2 の名前は %q_v2 の規約: got %q", p[0], p[1])
		}
	}
}

// absV2FreshCross: 長く低迷(200日線・126日前より下)→ 最終バーで両方を初めて上抜く。
func absV2FreshCross() []market.Candle {
	n := absTrendSMA + 2
	cs := make([]market.Candle, n)
	for i := 0; i < n-1; i++ {
		// 緩やかな下落トレンド: 直近ほど安い → 最終バー直前は 200SMA も 126日前も下回る
		p := 2000 - float64(i)
		cs[i] = market.Candle{Open: p, High: p + 5, Low: p - 5, Close: p, Volume: 1000}
	}
	// 最終バーで急騰して 200日線・126日前の両方を上抜く
	jump := 2500.0
	cs[n-1] = market.Candle{Open: 1900, High: jump + 5, Low: 1900, Close: jump, Volume: 3000}
	return cs
}

// atrV2Compressed: 前半が荒く直近が凪(= 圧縮)→ 最終バーで +1ATR 超の拡大。
func atrV2Compressed() []market.Candle {
	n := atrTrendSMA + 2
	cs := make([]market.Candle, n)
	base := 1000.0
	for i := 0; i < n-1; i++ {
		p := base + float64(i)*2
		w := 40.0 // 荒い期間
		if i >= n-1-(atrExitPeriod+1) {
			w = 1.0 // 直近は凪 = 圧縮
		}
		cs[i] = market.Candle{Open: p, High: p + w, Low: p - w, Close: p, Volume: 1000}
	}
	prev := cs[n-2].Close
	last := prev + 60 // 直近 ATR(≈1〜2)を大きく超える拡大
	cs[n-1] = market.Candle{Open: prev, High: last, Low: prev, Close: last, Volume: 3000}
	return cs
}

// atrV2NotCompressed: 直近ほど荒い(ATR14 >= ATR50)→ 圧縮していない拡大。
func atrV2NotCompressed() []market.Candle {
	n := atrTrendSMA + 2
	cs := make([]market.Candle, n)
	base := 1000.0
	for i := 0; i < n-1; i++ {
		p := base + float64(i)*2
		w := 1.0
		if i >= n-1-(atrExitPeriod+1) {
			w = 40.0 // 直近が荒い = 圧縮なし
		}
		cs[i] = market.Candle{Open: p, High: p + w, Low: p - w, Close: p, Volume: 1000}
	}
	prev := cs[n-2].Close
	last := prev + 200 // 直近 ATR(大きい)をさらに超える拡大
	cs[n-1] = market.Candle{Open: prev, High: last, Low: prev, Close: last, Volume: 3000}
	return cs
}

// dbV2FirstDay: 平坦なチャネル → 最終バーで初めて 20日高値を上抜く。
func dbV2FirstDay() []market.Candle {
	n := dbWindow + atrExitPeriod + 3
	cs := make([]market.Candle, n)
	for i := 0; i < n-1; i++ {
		cs[i] = market.Candle{Open: 1000, High: 1010, Low: 990, Close: 1000, Volume: 1000}
	}
	cs[n-1] = market.Candle{Open: 1000, High: 1100, Low: 1000, Close: 1090, Volume: 3000}
	return cs
}

// dbV2SecondDay: 前バーで既にチャネルを抜けており、最終バーはその継続(2日目)。
func dbV2SecondDay() []market.Candle {
	cs := dbV2FirstDay()
	n := len(cs)
	// 前バー = ブレイク初日、最終バー = さらに上(2日目)
	cs = append(cs, market.Candle{Open: 1090, High: 1150, Low: 1090, Close: 1140, Volume: 3000})
	_ = n
	return cs
}
