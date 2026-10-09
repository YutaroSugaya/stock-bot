package strategy

import (
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

// bnfPanicSeries: 29 calm bars then a sharp crash on >=1.5x volume.
func bnfPanicSeries() []market.Candle {
	cs := make([]market.Candle, 30)
	for i := 0; i < 29; i++ {
		cs[i] = market.Candle{Open: 2000, High: 2010, Low: 1990, Close: 2000, Volume: 1000}
	}
	cs[29] = market.Candle{Open: 1990, High: 2000, Low: 1690, Close: 1700, Volume: 2200} // -15% crash, 2.2x vol
	return cs
}

func TestBNFReversion_EntersOnPanicCrash_Capped(t *testing.T) {
	sig := BNFReversion{}.Evaluate(candIn(config.StrategyBNFReversion, bnfPanicSeries(), 1))
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("want BUY on panic crash, got decision=%q side=%q reason=%q", sig.Decision, sig.Side, sig.Reason)
	}
	if sig.TakeProfitJPY <= 0 {
		t.Fatal("capped arm must set a reversion take-profit toward the 25MA")
	}
	if sig.RatchetArmJPY != 0 {
		t.Fatal("capped arm must not arm a ratchet")
	}
}

// time-stop は **10 営業日目の引け前(14:50)**。
// 取引時間を持たない呼び手でも期限は付く(週末だけを飛ばす同時刻)— 無期限に縮退させない。
func TestBNFReversion_MaxHoldIsTenBusinessDays(t *testing.T) {
	in := candIn(config.StrategyBNFReversion, bnfPanicSeries(), 1)
	in.Now = time.Date(2026, 9, 14, 0, 1, 0, 0, time.UTC) // 月曜・Hours は zero 値
	sig := BNFReversion{}.Evaluate(in)
	if !sig.IsEntry() {
		t.Fatalf("precondition: want ENTER, got decision=%q reason=%q", sig.Decision, sig.Reason)
	}
	if got, want := sig.MaxHoldMinutes, 14*1440; got != want {
		t.Fatalf("BNF time-stop = %d min, want %d(10 営業日 = 週末 2 回を跨いで 14 暦日)", got, want)
	}
}

func TestBNFReversionTrail_EntersOnPanicCrash_Uncapped(t *testing.T) {
	sig := BNFReversionTrail{}.Evaluate(candIn(config.StrategyBNFReversionTrail, bnfPanicSeries(), 1))
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("want BUY, got decision=%q side=%q reason=%q", sig.Decision, sig.Side, sig.Reason)
	}
	if sig.TakeProfitJPY != 0 {
		t.Fatal("trail arm must be uncapped (TP=0)")
	}
	if sig.RatchetArmJPY <= 0 || sig.RatchetGivebackJPY <= 0 {
		t.Fatal("trail arm must set an ATR ratchet")
	}
}

func TestBNF_NoTradeWithoutCrash(t *testing.T) {
	cs := make([]market.Candle, 30)
	for i := range cs {
		cs[i] = market.Candle{Open: 2000, High: 2010, Low: 1990, Close: 2000, Volume: 1000}
	}
	sig := BNFReversion{}.Evaluate(candIn(config.StrategyBNFReversion, cs, 1))
	if sig.Decision == DecisionEnter {
		t.Fatalf("calm market should be NO_TRADE, got %+v", sig)
	}
}

func TestBNF_NoTradeWithoutVolume(t *testing.T) {
	// a crash but on LOW volume (no panic) must not trigger.
	cs := bnfPanicSeries()
	cs[29].Volume = 1000 // same as average -> volRatio ~1.0 < 1.5
	sig := BNFReversion{}.Evaluate(candIn(config.StrategyBNFReversion, cs, 1))
	if sig.Decision == DecisionEnter {
		t.Fatalf("crash without volume should be NO_TRADE, got %+v", sig)
	}
}
