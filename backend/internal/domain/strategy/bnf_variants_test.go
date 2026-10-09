package strategy

import (
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/ta"
)

// ---- fixtures ---------------------------------------------------------------

// bnfDay2SetupSeries: 29 本の凪 + 前日は「出来高 2.2 倍だが −5%(まだ −12% に届かない)」。
// day2 の前日条件(出来高 ≥ 1.5× かつ 乖離 > −12%)だけが成立する系列。
func bnfDay2SetupSeries() []market.Candle {
	cs := make([]market.Candle, 30)
	for i := 0; i < 29; i++ {
		cs[i] = market.Candle{Open: 2000, High: 2010, Low: 1990, Close: 2000, Volume: 1000}
	}
	cs[29] = market.Candle{Open: 1990, High: 2000, Low: 1890, Close: 1900, Volume: 2200} // -5%, 2.2x vol
	return cs
}

// withLast は現在値だけを差し替える(前日までの日足はそのまま)。
func withLast(in EvalInput, last float64) EvalInput {
	in.Summary.CurrentRate.Last = last
	return in
}

// ---- A. bnf_day2_reversion --------------------------------------------------

func TestBNFDay2_EntersWhenIntradayPriceCrossesThePanicLine(t *testing.T) {
	in := withLast(candIn(config.StrategyBNFDay2Reversion, bnfDay2SetupSeries(), 1), 1700) // -15% vs 25MA
	sig := BNFDay2Reversion{}.Evaluate(in)
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("want BUY, got decision=%q side=%q reason=%q", sig.Decision, sig.Side, sig.Reason)
	}
	if sig.EntryPrice != 1700 {
		t.Fatalf("entry must be the intraday price (1700), got %v", sig.EntryPrice)
	}
	if sig.TakeProfitJPY <= 0 {
		t.Fatal("capped arm must set a reversion take-profit toward the 25MA")
	}
	if sig.RatchetArmJPY != 0 || sig.RatchetGivebackJPY != 0 {
		t.Fatal("capped arm must not arm a ratchet")
	}
	bnf := BNFReversion{}.Evaluate(withLast(candIn(config.StrategyBNFReversion, bnfPanicSeries(), 1), 1700))
	if bnf.IsEntry() && sig.MaxHoldMinutes/1440 != bnf.MaxHoldMinutes/1440 {
		t.Fatalf("MaxHold must be borrowed from BNF (10 business days): got %d, want ~%d", sig.MaxHoldMinutes, bnf.MaxHoldMinutes)
	}
	if sig.MaxHoldMinutes < 10*1440 {
		t.Fatalf("MaxHold = %d, want >= 10 営業日(>= 10 暦日)", sig.MaxHoldMinutes)
	}
	if sig.HoldingMode != order.HoldingMultiday {
		t.Fatalf("holding mode = %q, want multiday (same as BNF)", sig.HoldingMode)
	}
	if sig.StrategyName != config.StrategyBNFDay2Reversion {
		t.Fatalf("strategy name = %q", sig.StrategyName)
	}
}

// 出口幾何は BNF から**そのまま借りる**(新しい係数を 1 つも選ばない)。
func TestBNFDay2_ExitGeometryIsBorrowedFromBNF(t *testing.T) {
	d := bnfDay2SetupSeries()
	in := withLast(candIn(config.StrategyBNFDay2Reversion, d, 1), 1700)
	sig := BNFDay2Reversion{}.Evaluate(in)
	if !sig.IsEntry() {
		t.Fatalf("precondition: want ENTER, got %q/%q", sig.Decision, sig.Reason)
	}
	sma := smaOfCloses(d, bnfSMA)
	atr := ta.ATR(d, bnfATRPeriod)
	wantTP, wantSL := bnfReversionExitJPY(1700, sma, bnfStopATR, atr)
	if sig.TakeProfitJPY != wantTP || sig.StopLossJPY != wantSL {
		t.Fatalf("exit = (tp %v, sl %v), want BNF geometry (tp %v, sl %v)", sig.TakeProfitJPY, sig.StopLossJPY, wantTP, wantSL)
	}
}

func TestBNFDay2_NoTradeWhileIntradayPriceIsAboveThePanicLine(t *testing.T) {
	in := withLast(candIn(config.StrategyBNFDay2Reversion, bnfDay2SetupSeries(), 1), 1850) // -7.5%: not yet
	sig := BNFDay2Reversion{}.Evaluate(in)
	if sig.IsEntry() || sig.Reason != "no_intraday_panic" {
		t.Fatalf("want no_intraday_panic, got decision=%q reason=%q", sig.Decision, sig.Reason)
	}
}

func TestBNFDay2_NoTradeWithoutPrevDayVolumeSpike(t *testing.T) {
	d := bnfDay2SetupSeries()
	d[29].Volume = 1000 // 1.0x: no panic volume yesterday
	in := withLast(candIn(config.StrategyBNFDay2Reversion, d, 1), 1700)
	sig := BNFDay2Reversion{}.Evaluate(in)
	if sig.IsEntry() || sig.Reason != "no_day2_setup" {
		t.Fatalf("want no_day2_setup, got decision=%q reason=%q", sig.Decision, sig.Reason)
	}
}

// 前日の終値で既に −12% を割っていた銘柄は bnf の領分(day2 は「当日割った」だけを取る)。
func TestBNFDay2_NoTradeWhenYesterdayAlreadyPanicked(t *testing.T) {
	in := withLast(candIn(config.StrategyBNFDay2Reversion, bnfPanicSeries(), 1), 1650)
	sig := BNFDay2Reversion{}.Evaluate(in)
	if sig.IsEntry() || sig.Reason != "no_day2_setup" {
		t.Fatalf("want no_day2_setup, got decision=%q reason=%q", sig.Decision, sig.Reason)
	}
}

func TestBNFDay2_NoTradeWithoutPrice(t *testing.T) {
	in := withLast(candIn(config.StrategyBNFDay2Reversion, bnfDay2SetupSeries(), 1), 0)
	sig := BNFDay2Reversion{}.Evaluate(in)
	if sig.IsEntry() || sig.Reason != "no_price" {
		t.Fatalf("want no_price, got decision=%q reason=%q", sig.Decision, sig.Reason)
	}
}

func TestBNFDay2_LongOnly(t *testing.T) {
	in := withLast(candIn(config.StrategyBNFDay2Reversion, bnfDay2SetupSeries(), 1), 1700)
	in.Config.Entry.Direction = config.DirectionSellOnly
	sig := BNFDay2Reversion{}.Evaluate(in)
	if sig.IsEntry() || sig.Reason != "direction_sell_only" {
		t.Fatalf("want direction_sell_only, got decision=%q reason=%q", sig.Decision, sig.Reason)
	}
}

func TestBNFDay2Trail_SameEntryUncappedExit(t *testing.T) {
	d := bnfDay2SetupSeries()
	capped := BNFDay2Reversion{}.Evaluate(withLast(candIn(config.StrategyBNFDay2Reversion, d, 1), 1700))
	trail := BNFDay2ReversionTrail{}.Evaluate(withLast(candIn(config.StrategyBNFDay2ReversionTrail, d, 1), 1700))
	if !trail.IsEntry() || trail.Side != order.SideBuy {
		t.Fatalf("want BUY, got decision=%q reason=%q", trail.Decision, trail.Reason)
	}
	if trail.TakeProfitJPY != 0 {
		t.Fatal("trail arm must be uncapped (TP=0)")
	}
	atr := ta.ATR(d, bnfATRPeriod)
	if trail.RatchetArmJPY != bnfTrailArmATR*atr || trail.RatchetGivebackJPY != bnfTrailGiveATR*atr {
		t.Fatalf("ratchet = (%v, %v), want BNF trail (%v, %v)", trail.RatchetArmJPY, trail.RatchetGivebackJPY, bnfTrailArmATR*atr, bnfTrailGiveATR*atr)
	}
	// 入口(建値・SL・MaxHold)は兄弟と同一。
	if trail.EntryPrice != capped.EntryPrice || trail.StopLossJPY != capped.StopLossJPY || trail.MaxHoldMinutes != capped.MaxHoldMinutes {
		t.Fatalf("entry differs from sibling: trail=%+v capped=%+v", trail, capped)
	}
	if trail.StrategyName != config.StrategyBNFDay2ReversionTrail {
		t.Fatalf("strategy name = %q", trail.StrategyName)
	}
	// 入口ゲートで落ちる理由も兄弟と同じ。
	no := BNFDay2ReversionTrail{}.Evaluate(withLast(candIn(config.StrategyBNFDay2ReversionTrail, d, 1), 1850))
	if no.IsEntry() || no.Reason != "no_intraday_panic" {
		t.Fatalf("trail gate reason = %q, want no_intraday_panic", no.Reason)
	}
}

// ---- B. bnf_stabilized_reversion -------------------------------------------

// 前日終値以上で評価された日は **bnf と完全に同じ建玉**になる(ゲート以外は 1 ビットも違わない)。
func TestBNFStabilized_IsIdenticalToBNFWhenPriceHoldsThePrevClose(t *testing.T) {
	d := bnfPanicSeries() // prev close 1700
	for _, last := range []float64{1700, 1750} {
		base := BNFReversion{}.Evaluate(withLast(candIn(config.StrategyBNFReversion, d, 1), last))
		got := BNFStabilizedReversion{}.Evaluate(withLast(candIn(config.StrategyBNFStabilizedReversion, d, 1), last))
		if !got.IsEntry() || got.Side != order.SideBuy {
			t.Fatalf("last=%v: want BUY, got decision=%q reason=%q", last, got.Decision, got.Reason)
		}
		if got.StrategyName != config.StrategyBNFStabilizedReversion {
			t.Fatalf("strategy name = %q", got.StrategyName)
		}
		got.StrategyName, base.StrategyName = "", ""
		got.Reason, base.Reason = "", ""
		got.CreatedAt, base.CreatedAt = time.Time{}, time.Time{}
		if got != base {
			t.Fatalf("last=%v: signal differs from bnf_reversion:\n got=%+v\nbase=%+v", last, got, base)
		}
	}
}

func TestBNFStabilized_SkipsWhilePriceIsBelowThePrevClose(t *testing.T) {
	in := withLast(candIn(config.StrategyBNFStabilizedReversion, bnfPanicSeries(), 1), 1690) // < prev close 1700
	sig := BNFStabilizedReversion{}.Evaluate(in)
	if sig.IsEntry() || sig.Reason != "still_falling" {
		t.Fatalf("want still_falling, got decision=%q reason=%q", sig.Decision, sig.Reason)
	}
}

// ゲートは bnf の入口条件の**後**に置く: パニックでない日に still_falling を数えると
// signal_rejections の分布が bnf と比較できなくなる。
func TestBNFStabilized_NoPanicReasonMatchesBNF(t *testing.T) {
	cs := make([]market.Candle, 30)
	for i := range cs {
		cs[i] = market.Candle{Open: 2000, High: 2010, Low: 1990, Close: 2000, Volume: 1000}
	}
	in := withLast(candIn(config.StrategyBNFStabilizedReversion, cs, 1), 1900) // below prev close but no panic
	sig := BNFStabilizedReversion{}.Evaluate(in)
	if sig.IsEntry() || sig.Reason != "no_panic_crash" {
		t.Fatalf("want no_panic_crash (same as bnf), got decision=%q reason=%q", sig.Decision, sig.Reason)
	}
}

func TestBNFStabilized_NoTradeWithoutPrice(t *testing.T) {
	in := withLast(candIn(config.StrategyBNFStabilizedReversion, bnfPanicSeries(), 1), 0)
	sig := BNFStabilizedReversion{}.Evaluate(in)
	if sig.IsEntry() || sig.Reason != "no_price" {
		t.Fatalf("want no_price, got decision=%q reason=%q", sig.Decision, sig.Reason)
	}
}

func TestBNFStabilizedTrail_IsIdenticalToBNFTrailWhenPriceHoldsThePrevClose(t *testing.T) {
	d := bnfPanicSeries()
	base := BNFReversionTrail{}.Evaluate(withLast(candIn(config.StrategyBNFReversionTrail, d, 1), 1700))
	got := BNFStabilizedReversionTrail{}.Evaluate(withLast(candIn(config.StrategyBNFStabilizedReversionTrail, d, 1), 1700))
	if !got.IsEntry() || got.TakeProfitJPY != 0 || got.RatchetArmJPY <= 0 {
		t.Fatalf("want uncapped BUY, got %+v", got)
	}
	if got.StrategyName != config.StrategyBNFStabilizedReversionTrail {
		t.Fatalf("strategy name = %q", got.StrategyName)
	}
	got.StrategyName, base.StrategyName = "", ""
	got.Reason, base.Reason = "", ""
	got.CreatedAt, base.CreatedAt = time.Time{}, time.Time{}
	if got != base {
		t.Fatalf("signal differs from bnf_reversion_trail:\n got=%+v\nbase=%+v", got, base)
	}
	no := BNFStabilizedReversionTrail{}.Evaluate(withLast(candIn(config.StrategyBNFStabilizedReversionTrail, d, 1), 1690))
	if no.IsEntry() || no.Reason != "still_falling" {
		t.Fatalf("trail gate reason = %q, want still_falling", no.Reason)
	}
}

// ---- screeners ---------------------------------------------------------------

func TestScreen_BNFDay2_TriggersOnVolumeSpikeShortOfThePanicLine(t *testing.T) {
	setup := BNFDay2Reversion{}.Screen("7203", bnfDay2SetupSeries())
	if !setup.Triggered {
		t.Fatalf("day2 setup (2.2x vol, -5%%) must trigger: %+v", setup)
	}
	if setup.Strategy != config.StrategyBNFDay2Reversion {
		t.Fatalf("Strategy = %q", setup.Strategy)
	}
	if setup.StopLossJPY <= 0 {
		t.Fatal("planned stop (2.0×ATR) must be reported so the selector can weigh it before handing out a slot")
	}
	// 前日に既に −12% を割っている = bnf の領分。day2 では発火しない。
	if p := (BNFDay2Reversion{}).Screen("7203", bnfPanicSeries()); p.Triggered {
		t.Fatalf("already-panicked series must not trigger day2: %+v", p)
	}
	// 出来高が無ければ発火しない。
	d := bnfDay2SetupSeries()
	d[29].Volume = 1000
	if q := (BNFDay2Reversion{}).Screen("7203", d); q.Triggered {
		t.Fatalf("no volume spike must not trigger day2: %+v", q)
	}
	// 兄弟は入口(Triggered / Score)が一致し、名前と Detail だけ違う。
	trail := BNFDay2ReversionTrail{}.Screen("7203", bnfDay2SetupSeries())
	if trail.Triggered != setup.Triggered || trail.Score != setup.Score || trail.Strategy != config.StrategyBNFDay2ReversionTrail {
		t.Fatalf("trail screen differs from base: base=%+v trail=%+v", setup, trail)
	}
	if trail.Detail == setup.Detail {
		t.Errorf("Detail が同一 — 出口の違いが読めない: %q", trail.Detail)
	}
}

// −12% に近い(乖離が深い)候補ほど当日に割る確率が高いので上に並ぶ。新しい定数は使わない
// (bnf と同じ min(dev/閾値, vol/閾値))。
func TestScreen_BNFDay2_RanksDeeperDeviationFirst(t *testing.T) {
	shallow := bnfDay2SetupSeries()
	shallow[29].Close = 1960     // -2%
	deep := bnfDay2SetupSeries() // -5%
	if (BNFDay2Reversion{}).Screen("A", deep).Score <= (BNFDay2Reversion{}).Screen("B", shallow).Score {
		t.Fatal("deeper deviation must rank first")
	}
}

func TestScreen_BNFStabilized_MirrorsBNFGate(t *testing.T) {
	for _, d := range [][]market.Candle{bnfPanicSeries(), bnfDay2SetupSeries()} {
		base := BNFReversion{}.Screen("7203", d)
		got := BNFStabilizedReversion{}.Screen("7203", d)
		trail := BNFStabilizedReversionTrail{}.Screen("7203", d)
		if got.Triggered != base.Triggered || got.Score != base.Score || got.StopLossJPY != base.StopLossJPY {
			t.Fatalf("stabilized screen must mirror bnf gate: base=%+v got=%+v", base, got)
		}
		if got.Strategy != config.StrategyBNFStabilizedReversion || trail.Strategy != config.StrategyBNFStabilizedReversionTrail {
			t.Fatalf("strategy labels: %q / %q", got.Strategy, trail.Strategy)
		}
		if trail.Triggered != base.Triggered || trail.Score != base.Score {
			t.Fatalf("stabilized trail screen must mirror bnf gate: base=%+v trail=%+v", base, trail)
		}
	}
}

// ---- helpers ------------------------------------------------------------------

func smaOfCloses(d []market.Candle, n int) float64 {
	s := 0.0
	for _, c := range d[len(d)-n:] {
		s += c.Close
	}
	return s / float64(n)
}
