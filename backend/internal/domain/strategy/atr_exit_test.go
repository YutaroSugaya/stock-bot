package strategy

import (
	"math"
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/ta"
)

// 出口の ATR 化を固定する。入口は一切触っていない。
// 係数は教科書既定の翻訳で、採点結果を見て動かすことは事前登録で禁じられている。

// 価格を k 倍すると ATR も k 倍 — 出口幅の比例性を見るのに使う。
func scaleCandles(cs []market.Candle, k float64) []market.Candle {
	out := make([]market.Candle, len(cs))
	for i, c := range cs {
		out[i] = market.Candle{Open: c.Open * k, High: c.High * k, Low: c.Low * k,
			Close: c.Close * k, Volume: c.Volume}
	}
	return out
}

// ① config 系: TP/SL が ATR に比例し、cfg.Exit の円幅(TP=100 / SL=50)は使われない。
func TestConfigExitStrategiesScaleWithATR(t *testing.T) {
	cases := []struct {
		name  config.StrategyName
		strat Strategy
		daily []market.Candle
	}{
		{config.StrategyAbsMomentum, AbsMomentum{}, steadyUp(absTrendSMA + 2)},
		{config.StrategyHigh52wMomentum, High52wMomentum{}, popLast(steadyUp(h52Window + 2))},
		{config.StrategyDonchianBreakout, DonchianBreakout{}, popLast(steadyUp(dbWindow + 2))},
		{config.StrategyATRBreakout, ATRBreakout{}, popLast(steadyUp(atrTrendSMA + 2))},
		{config.StrategyHighVolumePremium, HighVolumePremium{}, hvpSeries()},
		{config.StrategyPostJumpDrift, PostJumpDrift{}, pjdSeries()},
	}
	for _, tc := range cases {
		t.Run(string(tc.name), func(t *testing.T) {
			base := tc.strat.Evaluate(candIn(tc.name, tc.daily, 1))
			if !base.IsEntry() {
				t.Fatalf("前提: ENTER が要る。decision=%q reason=%q", base.Decision, base.Reason)
			}
			atr := ta.ATR(tc.daily, atrExitPeriod)
			if atr <= 0 {
				t.Fatalf("前提: ATR>0 が要る (%v)", atr)
			}
			if got, want := base.TakeProfitJPY, atrExitTPMult*atr; math.Abs(got-want) > 1e-9 {
				t.Errorf("TP = %v, want %v (= %.1f×ATR)。cfg.Exit の円幅は使わない", got, want, atrExitTPMult)
			}
			if got, want := base.StopLossJPY, atrExitSLMult*atr; math.Abs(got-want) > 1e-9 {
				t.Errorf("SL = %v, want %v (= %.1f×ATR)", got, want, atrExitSLMult)
			}
			// 2 銘柄目 = 価格 3 倍のスケール違い。ATR も 3 倍 → 出口幅も 3 倍。
			scaled := tc.strat.Evaluate(candIn(tc.name, scaleCandles(tc.daily, 3), 1))
			if !scaled.IsEntry() {
				t.Fatalf("スケール後も ENTER のはず: reason=%q", scaled.Reason)
			}
			if got := scaled.TakeProfitJPY / base.TakeProfitJPY; math.Abs(got-3) > 1e-6 {
				t.Errorf("TP のスケール比 = %v, want 3(ATR 比例していない)", got)
			}
			if got := scaled.StopLossJPY / base.StopLossJPY; math.Abs(got-3) > 1e-6 {
				t.Errorf("SL のスケール比 = %v, want 3", got)
			}
		})
	}
}

// ATR が取れない(履歴不足で 0)ときは黙って cfg の円幅に落ちず no_trade。
func TestConfigExitStrategiesRejectWhenNoATR(t *testing.T) {
	// abs_momentum の入口は通しつつ直近 15 本を同値にして ATR(14)=0 を作る(落ちる理由を no_atr に絞る)。
	n := absTrendSMA + 2
	flat := make([]market.Candle, n)
	for i := 0; i < n; i++ {
		p := 1000 + float64(i)*3
		if i >= n-(atrExitPeriod+1) {
			p = 1000 + float64(n-(atrExitPeriod+1))*3
		}
		flat[i] = market.Candle{Open: p, High: p, Low: p, Close: p}
	}
	sig := AbsMomentum{}.Evaluate(candIn(config.StrategyAbsMomentum, flat, 1))
	if sig.IsEntry() {
		t.Fatalf("ATR=0 で ENTER してはいけない: TP=%v SL=%v", sig.TakeProfitJPY, sig.StopLossJPY)
	}
	if sig.Reason != "no_atr" {
		t.Fatalf("reason = %q, want no_atr", sig.Reason)
	}
}

// ② BNF: SL が固定 8% ではなく 2.0×ATR。TP(25MA までの距離)は不変。
func TestBNFReversion_StopIsATRScaled(t *testing.T) {
	d := bnfPanicSeries()
	sig := BNFReversion{}.Evaluate(candIn(config.StrategyBNFReversion, d, 1))
	if !sig.IsEntry() {
		t.Fatalf("前提: ENTER が要る: %q", sig.Reason)
	}
	atr := ta.ATR(d, bnfATRPeriod)
	if got, want := sig.StopLossJPY, bnfStopATR*atr; math.Abs(got-want) > 1e-9 {
		t.Fatalf("SL = %v, want %v (= %.1f×ATR)。固定 8%% は廃止", got, want, bnfStopATR)
	}
	if got := sig.EntryPrice * 0.08; math.Abs(sig.StopLossJPY-got) < 1e-9 {
		t.Fatalf("SL が旧・固定8%%(%v)のまま", got)
	}
	// TP は 25MA までの距離のまま(入口も TP も不変 — 変えたのは SL だけ)。
	if sig.TakeProfitJPY <= 0 {
		t.Fatal("capped arm の TP(25MA までの距離)は不変のはず")
	}
}

// ③ trail: arm 後の giveback 線(peak−1.5ATR)が SL(entry−2.0ATR)より内側 = トレールが実際に効く。
func TestBNFReversionTrail_GivebackIsInsideStop(t *testing.T) {
	d := bnfPanicSeries()
	sig := BNFReversionTrail{}.Evaluate(candIn(config.StrategyBNFReversionTrail, d, 1))
	if !sig.IsEntry() {
		t.Fatalf("前提: ENTER が要る: %q", sig.Reason)
	}
	atr := ta.ATR(d, bnfATRPeriod)
	if got, want := sig.RatchetArmJPY, bnfTrailArmATR*atr; math.Abs(got-want) > 1e-9 {
		t.Fatalf("arm = %v, want %v(arm は不変 1.0×ATR)", got, want)
	}
	if got, want := sig.RatchetGivebackJPY, bnfTrailGiveATR*atr; math.Abs(got-want) > 1e-9 {
		t.Fatalf("giveback = %v, want %v (= %.1f×ATR)", got, want, bnfTrailGiveATR)
	}
	if sig.TakeProfitJPY != 0 {
		t.Fatal("trail arm は uncapped のまま(TP=0)")
	}
	// 幾何の本体: 最悪ケース(peak = arm ちょうど)でも giveback 線が SL より上。
	worstPeak := sig.EntryPrice + sig.RatchetArmJPY
	givebackLine := worstPeak - sig.RatchetGivebackJPY
	stopLine := sig.EntryPrice - sig.StopLossJPY
	if givebackLine <= stopLine {
		t.Fatalf("giveback 線 %v が SL 線 %v の外側 — トレールが効かない(2026-08-03 追補の再発)",
			givebackLine, stopLine)
	}
}

// bnf_intraday_reversion はバックテストの凍結パラメータ(変更 = 事前登録の無効化)。
func TestBNFIntradayParametersAreFrozen(t *testing.T) {
	snapshot := map[string]float64{
		"bnfiMinBars":    bnfiMinBars,
		"bnfiTPFrac":     bnfiTPFrac,
		"bnfiSLCapPct":   bnfiSLCapPct,
		"bnfiMaxHoldMin": bnfiMaxHoldMin,
	}
	want := map[string]float64{
		"bnfiMinBars":    3,
		"bnfiTPFrac":     0.3,
		"bnfiSLCapPct":   0.02,
		"bnfiMaxHoldMin": 300,
	}
	for k, w := range want {
		if got := snapshot[k]; got != w {
			t.Fatalf("%s = %v, want %v — バックテストで凍結したパラメータ。変更すると事前登録が無効になる", k, got, w)
		}
	}
}

// ④ cost floor(edge gate)は ATR 出口でも効く。
func TestATRExitStillPassesThroughCostFloor(t *testing.T) {
	d := popLast(steadyUp(atrTrendSMA + 2))
	// spread を極端に広げると往復コスト床が TP(3×ATR)を超える。
	sig := ATRBreakout{}.Evaluate(candIn(config.StrategyATRBreakout, d, 10000))
	if sig.IsEntry() {
		t.Fatalf("コスト床を越えない提案は落とすはず: TP=%v", sig.TakeProfitJPY)
	}
	if sig.Reason == "" {
		t.Fatal("落とした理由を必ず残す")
	}
}

// メニュー外の euphoria short も同じ幾何に追随させる(片方だけ ATR 化すると前提が黙って崩れる)。
func TestBNFEuphoriaShort_StopIsATRScaled(t *testing.T) {
	d := bnfEuphoriaSeries()
	sig := BNFEuphoriaShort{}.Evaluate(candIn(config.StrategyBNFEuphoriaShort, d, 1))
	if !sig.IsEntry() || sig.Side != order.SideSell {
		t.Fatalf("前提: SELL ENTER が要る: decision=%q reason=%q", sig.Decision, sig.Reason)
	}
	atr := ta.ATR(d, bnfATRPeriod)
	if got, want := sig.StopLossJPY, bnfStopATR*atr; math.Abs(got-want) > 1e-9 {
		t.Fatalf("SL = %v, want %v (= %.1f×ATR)", got, want, bnfStopATR)
	}
}

// bnfEuphoriaSeries: 29 calm bars then a sharp +15% spike on 2.2x volume.
func bnfEuphoriaSeries() []market.Candle {
	cs := make([]market.Candle, 30)
	for i := 0; i < 29; i++ {
		cs[i] = market.Candle{Open: 2000, High: 2010, Low: 1990, Close: 2000, Volume: 1000}
	}
	cs[29] = market.Candle{Open: 2010, High: 2310, Low: 2000, Close: 2300, Volume: 2200}
	return cs
}

// hvpSeries: 100SMA 超の上昇トレンド + 最終日が 50 日最大の出来高(GKM 発火)。
func hvpSeries() []market.Candle {
	cs := steadyUp(hvpTrendSMA + 2)
	for i := range cs {
		cs[i].Volume = 1000
	}
	cs[len(cs)-1].Volume = 100000
	return cs
}

// pjdSeries: 平穏な系列 + 最終日に大きな上ジャンプ(出来高スパイクつき)。
func pjdSeries() []market.Candle {
	n := peadVolWindow + 5
	cs := make([]market.Candle, n)
	p := 1000.0
	for i := 0; i < n-1; i++ {
		// 微小な上下でボラを作る(sigma>0 が要る)。
		if i%2 == 0 {
			p += 1
		} else {
			p -= 1
		}
		cs[i] = market.Candle{Open: p, High: p + 2, Low: p - 2, Close: p, Volume: 1000}
	}
	jump := p * 1.15
	cs[n-1] = market.Candle{Open: p, High: jump + 2, Low: p, Close: jump, Volume: 100000}
	return cs
}

// 係数はリテラルで固定する。自己参照の比例テストだけでは定数の書き換えを検出できない
// (実測: atrExitSLMult=0.0 で全 ENTER の SL が 0 になるのにテストは緑だった)。
func TestATRExitCoefficientsArePinned(t *testing.T) {
	if atrExitTPMult != 3.0 {
		t.Fatalf("atrExitTPMult = %v, want 3.0 — 事前登録値。結果を見てから動かさない", atrExitTPMult)
	}
	if atrExitSLMult != 2.0 {
		t.Fatalf("atrExitSLMult = %v, want 2.0 — 同上", atrExitSLMult)
	}
	if atrExitPeriod != 14 {
		t.Fatalf("atrExitPeriod = %v, want 14", atrExitPeriod)
	}
	if bnfStopATR != 2.0 {
		t.Fatalf("bnfStopATR = %v, want 2.0", bnfStopATR)
	}
	if bnfTrailArmATR != 1.0 || bnfTrailGiveATR != 1.5 {
		t.Fatalf("trail arm/giveback = %v/%v, want 1.0/1.5", bnfTrailArmATR, bnfTrailGiveATR)
	}
	// giveback は SL より内側でなければトレールが作動しない(再発防止)。
	if bnfTrailGiveATR >= bnfTrailArmATR+bnfStopATR {
		t.Fatalf("giveback %v >= arm %v + stop %v — giveback 線が SL の外側になりトレールが効かない",
			bnfTrailGiveATR, bnfTrailArmATR, bnfStopATR)
	}
}

// SL>0 を定数と無関係に検査する。比例テストは SL=0 で NaN 比較になり素通りし、paper broker も見ないのでここが最後の砦。
func TestEveryEntrySignalCarriesPositiveStop(t *testing.T) {
	cases := []struct {
		name  config.StrategyName
		strat Strategy
		daily []market.Candle
	}{
		{config.StrategyAbsMomentum, AbsMomentum{}, steadyUp(absTrendSMA + 2)},
		{config.StrategyAbsMomentumV2, AbsMomentumV2{}, absV2FreshCross()},
		{config.StrategyHigh52wMomentum, High52wMomentum{}, popLast(steadyUp(h52Window + 2))},
		{config.StrategyDonchianBreakout, DonchianBreakout{}, popLast(steadyUp(dbWindow + 2))},
		{config.StrategyDonchianBreakoutV2, DonchianBreakoutV2{}, dbV2FirstDay()},
		{config.StrategyATRBreakout, ATRBreakout{}, popLast(steadyUp(atrTrendSMA + 2))},
		{config.StrategyATRBreakoutV2, ATRBreakoutV2{}, atrV2Compressed()},
		{config.StrategyHighVolumePremium, HighVolumePremium{}, hvpSeries()},
		{config.StrategyPostJumpDrift, PostJumpDrift{}, pjdSeries()},
		{config.StrategyBNFReversion, BNFReversion{}, bnfPanicSeries()},
		{config.StrategyBNFReversionTrail, BNFReversionTrail{}, bnfPanicSeries()},
	}
	for _, tc := range cases {
		sig := tc.strat.Evaluate(candIn(tc.name, tc.daily, 1))
		if !sig.IsEntry() {
			continue // 入口が成立しないケースはこのテストの対象外
		}
		if !(sig.StopLossJPY > 0) {
			t.Errorf("%s: ENTER なのに SL=%v — SL 欠落は致命的(CLAUDE.md)", tc.name, sig.StopLossJPY)
		}
		// TP は trail 系が意図的に 0(uncapped)。負だけは許さない。
		if sig.TakeProfitJPY < 0 {
			t.Errorf("%s: TP が負 (%v)", tc.name, sig.TakeProfitJPY)
		}
	}
}
