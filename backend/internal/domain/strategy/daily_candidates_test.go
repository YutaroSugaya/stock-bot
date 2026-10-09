package strategy

import (
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

func candCfg(name config.StrategyName) *config.StrategyConfig {
	c := &config.StrategyConfig{ConfigID: "cfg", Symbol: "7203", StrategyName: name, HoldingMode: order.HoldingMultiday}
	c.Exit.TakeProfitJPY = 100
	c.Exit.StopLossJPY = 50
	c.Risk.Quantity = 100
	c.Entry.Direction = config.DirectionBoth
	return c
}

func candIn(name config.StrategyName, daily []market.Candle, spread float64) EvalInput {
	last := daily[len(daily)-1].Close
	return EvalInput{
		Now: time.Now(), Config: candCfg(name), CandlesDaily: daily,
		Summary: &market.MarketSummary{
			Symbol: "7203", TickSize: 1,
			CurrentRate: market.CurrentRate{Last: last, SpreadTicks: spread},
		},
	}
}

// steadyUp is a clean uptrend; each bar a small range around the close.
func steadyUp(n int) []market.Candle {
	cs := make([]market.Candle, n)
	for i := 0; i < n; i++ {
		p := 1000 + float64(i)*3
		cs[i] = market.Candle{Open: p, High: p + 3, Low: p - 3, Close: p}
	}
	return cs
}

// popLast forces a sharp final breakout bar (clears any recent channel + ATR).
func popLast(cs []market.Candle) []market.Candle {
	n := len(cs)
	p := cs[n-2].Close + 60
	cs[n-1] = market.Candle{Open: cs[n-2].Close, High: p, Low: cs[n-2].Close, Close: p}
	return cs
}

func allCandidates() []Strategy {
	return []Strategy{
		MACross{}, AbsMomentum{}, High52wMomentum{}, DonchianBreakout{},
		ATRBreakout{}, RSI2Reversion{}, BollingerReversion{}, GapReversion{},
	}
}

// 各候補が現実的な系列で構造的に妥当な signal を返すこと(ENTER なら IsEntry + HoldingMode)。
func TestCandidates_ValidSignalNoPanic(t *testing.T) {
	series := popLast(steadyUp(300))
	for _, s := range allCandidates() {
		sig := s.Evaluate(candIn(s.Name(), series, 1))
		if sig.Decision == DecisionEnter {
			if !sig.IsEntry() {
				t.Fatalf("%s: ENTER but not IsEntry: %+v", s.Name(), sig)
			}
			if sig.HoldingMode == "" {
				t.Fatalf("%s: ENTER with empty HoldingMode", s.Name())
			}
		}
		if sig.StrategyName != s.Name() {
			t.Fatalf("%s: signal name = %q", s.Name(), sig.StrategyName)
		}
	}
}

// TestCandidates_InsufficientHistory: too little history is always NO_TRADE.
func TestCandidates_InsufficientHistory(t *testing.T) {
	for _, s := range allCandidates() {
		sig := s.Evaluate(candIn(s.Name(), steadyUp(5), 1))
		if sig.Decision == DecisionEnter {
			t.Fatalf("%s: entered on 5-bar history: %+v", s.Name(), sig)
		}
	}
}

func TestAbsMomentum_BuysUptrend(t *testing.T) {
	sig := AbsMomentum{}.Evaluate(candIn(config.StrategyAbsMomentum, steadyUp(260), 1))
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("want BUY, got decision=%q side=%q reason=%q", sig.Decision, sig.Side, sig.Reason)
	}
}

func TestHigh52w_BuysNearHigh(t *testing.T) {
	sig := High52wMomentum{}.Evaluate(candIn(config.StrategyHigh52wMomentum, steadyUp(300), 1))
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("want BUY, got decision=%q side=%q reason=%q", sig.Decision, sig.Side, sig.Reason)
	}
}

func TestDonchianBreakout_BuysOnPop(t *testing.T) {
	sig := DonchianBreakout{}.Evaluate(candIn(config.StrategyDonchianBreakout, popLast(steadyUp(60)), 1))
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("want BUY, got decision=%q side=%q reason=%q", sig.Decision, sig.Side, sig.Reason)
	}
}

func TestATRBreakout_BuysOnPop(t *testing.T) {
	sig := ATRBreakout{}.Evaluate(candIn(config.StrategyATRBreakout, popLast(steadyUp(150)), 1))
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("want BUY, got decision=%q side=%q reason=%q", sig.Decision, sig.Side, sig.Reason)
	}
}

func TestBollinger_BuysBelowBand(t *testing.T) {
	cs := steadyUp(25)
	for i := range cs { // flatten so the band is tight
		cs[i] = market.Candle{Open: 2000, High: 2003, Low: 1997, Close: 2000}
	}
	cs[24] = market.Candle{Open: 2000, High: 2000, Low: 1900, Close: 1900}
	sig := BollingerReversion{}.Evaluate(candIn(config.StrategyBollingerReversion, cs, 1))
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("want BUY below band, got decision=%q side=%q reason=%q", sig.Decision, sig.Side, sig.Reason)
	}
}

func TestGapReversion_BuysDownGap(t *testing.T) {
	// 出口が ATR スケールになったので ATR(14) の履歴が要る。入口の検査内容は不変。
	cs := make([]market.Candle, 0, atrExitPeriod+2)
	for i := 0; i < atrExitPeriod+1; i++ {
		cs = append(cs, market.Candle{Open: 2000, High: 2010, Low: 1990, Close: 2000})
	}
	cs = append(cs, market.Candle{Open: 1900, High: 1910, Low: 1880, Close: 1895}) // -5% open gap
	sig := GapReversion{}.Evaluate(candIn(config.StrategyGapReversion, cs, 1))
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("want BUY on down-gap, got decision=%q side=%q reason=%q", sig.Decision, sig.Side, sig.Reason)
	}
}

func TestRSI2_BuysDipInUptrend(t *testing.T) {
	cs := steadyUp(210)
	// dip the final two closes so RSI(2)=0 while price stays above the 200SMA
	for _, i := range []int{208, 209} {
		c := cs[i-1].Close - 30
		cs[i] = market.Candle{Open: c + 1, High: c + 1, Low: c - 1, Close: c}
	}
	sig := RSI2Reversion{}.Evaluate(candIn(config.StrategyRSI2Reversion, cs, 1))
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("want BUY dip-in-uptrend, got decision=%q side=%q reason=%q", sig.Decision, sig.Side, sig.Reason)
	}
}

// バー毎に replay して BUY / SELL が一度でも出たかを見る(ma_cross の発火バーはデータ依存のため)。
func firesSomewhere(s Strategy, daily []market.Candle, minBars int) (buy, sell bool) {
	for i := minBars; i <= len(daily); i++ {
		sig := s.Evaluate(candIn(s.Name(), daily[:i], 1))
		if sig.IsEntry() {
			switch sig.Side {
			case order.SideBuy:
				buy = true
			case order.SideSell:
				sell = true
			}
		}
	}
	return
}

func baseThen(base float64, nFlat int, slope float64, nMove int) []market.Candle {
	cs := make([]market.Candle, 0, nFlat+nMove)
	for i := 0; i < nFlat; i++ {
		cs = append(cs, market.Candle{Open: base, High: base + 3, Low: base - 3, Close: base})
	}
	for k := 1; k <= nMove; k++ {
		p := base + slope*float64(k)
		cs = append(cs, market.Candle{Open: p, High: p + 3, Low: p - 3, Close: p})
	}
	return cs
}

func TestPostJumpDrift_FiresOnVolumeJump(t *testing.T) {
	// 70 low-vol bars (alternating ±0.5%) then a +10% jump on 4x volume.
	cs := make([]market.Candle, 71)
	for i := 0; i < 70; i++ {
		p := 2000.0
		if i%2 == 1 {
			p = 2010
		}
		cs[i] = market.Candle{Open: p, High: p + 2, Low: p - 2, Close: p, Volume: 1000}
	}
	prev := cs[69].Close
	jp := prev * 1.10
	cs[70] = market.Candle{Open: prev, High: jp, Low: prev, Close: jp, Volume: 4000}
	sig := PostJumpDrift{}.Evaluate(candIn(config.StrategyPostJumpDrift, cs, 1))
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("want BUY on volume jump, got decision=%q side=%q reason=%q", sig.Decision, sig.Side, sig.Reason)
	}
	// a calm series (no jump) must not fire.
	calm := make([]market.Candle, 71)
	for i := range calm {
		p := 2000.0
		if i%2 == 1 {
			p = 2010
		}
		calm[i] = market.Candle{Open: p, High: p + 2, Low: p - 2, Close: p, Volume: 1000}
	}
	if (PostJumpDrift{}).Evaluate(candIn(config.StrategyPostJumpDrift, calm, 1)).Decision == DecisionEnter {
		t.Fatal("calm series should not trigger post-jump drift")
	}
}

func TestMACross_FiresBothDirections(t *testing.T) {
	if buy, _ := firesSomewhere(MACross{}, baseThen(2000, 90, 25, 60), maSlow+2); !buy {
		t.Fatal("ma_cross should BUY on a golden cross (base -> rally)")
	}
	if _, sell := firesSomewhere(MACross{}, baseThen(3000, 90, -25, 60), maSlow+2); !sell {
		t.Fatal("ma_cross should SELL on a death cross (base -> selloff)")
	}
}

func TestCandidate_CostFloorRejects(t *testing.T) {
	sig := DonchianBreakout{}.Evaluate(candIn(config.StrategyDonchianBreakout, popLast(steadyUp(60)), 300))
	if sig.IsEntry() {
		t.Fatalf("huge spread should trip the cost floor, got entry: %+v", sig)
	}
	if sig.Reason != "tp_below_cost_floor" {
		t.Fatalf("reason = %q, want tp_below_cost_floor", sig.Reason)
	}
}
