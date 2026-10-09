package strategy

import (
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

// 最終バーが BNF パニック(終値 860 ≈ 25MA 比 -13.5%・出来高 5倍)の確定日足。パニック日は today の JST 前日。
func panicDaily(today time.Time, n int) []market.Candle {
	cs := make([]market.Candle, 0, n)
	day := today.AddDate(0, 0, -n)
	for i := 0; i < n; i++ {
		c := market.Candle{
			Symbol: "7203", OpenTime: day.AddDate(0, 0, i), Interval: 24 * time.Hour,
			Open: 1000, High: 1005, Low: 995, Close: 1000, Volume: 1000,
		}
		if i == n-1 {
			c.Open, c.High, c.Low, c.Close, c.Volume = 940, 950, 855, 860, 5000
		}
		cs = append(cs, c)
	}
	return cs
}

// now で終わる完成済み 5 分足を n 本。reversal=true なら最終バーが陽線かつ前バー高値超え(日中確認)。
func fiveMin(now time.Time, n int, reversal bool) []market.Candle {
	cs := make([]market.Candle, 0, n)
	start := now.Add(-time.Duration(n) * 5 * time.Minute)
	px := 850.0
	for i := 0; i < n; i++ {
		o := px
		c := px - 2 // drifting down (the morning continuation of the panic)
		hi, lo := o+1, c-1
		if reversal && i == n-1 {
			c = o + 6 // bullish bar...
			hi = c + 1
			lo = o - 1
			// ...closing above the prior bar's high (prev high = prev open+1).
		}
		cs = append(cs, market.Candle{
			Symbol: "7203", OpenTime: start.Add(time.Duration(i) * 5 * time.Minute),
			Interval: 5 * time.Minute, Open: o, High: hi, Low: lo, Close: c, Volume: 500,
		})
		px = c
	}
	return cs
}

func summaryAt(sym string, last float64, now time.Time) *market.MarketSummary {
	return &market.MarketSummary{
		Symbol: sym, GeneratedAt: now, TickSize: market.TickSize(last),
		CurrentRate: market.CurrentRate{Last: last, Bid: last, Ask: last + market.TickSize(last), SpreadTicks: 1, At: now},
	}
}

func bnfiInput(now time.Time, daily, m5 []market.Candle, last float64) EvalInput {
	cfg := &config.StrategyConfig{
		ConfigID: "cfg-bnfi", Symbol: "7203", StrategyName: config.StrategyBNFIntradayReversion,
	}
	cfg.Risk.Quantity = 100
	return EvalInput{
		Now:          now,
		Summary:      summaryAt("7203", last, now),
		CandlesDaily: daily,
		Candles5m:    m5,
		Config:       cfg,
	}
}

// 段階ウォッチ Tier A: 前日確定日足が BNF パニックの銘柄だけが当日の高頻度ウォッチ対象(当日の未確定バーでは hot にしない)。
func TestIntradayHotCandidate(t *testing.T) {
	loc := clock.JST
	now := time.Date(2026, 7, 31, 9, 30, 0, 0, loc)

	if !IntradayHotCandidate("7203", panicDaily(now, 40), now) {
		t.Fatal("前日パニックの銘柄は hot 候補になるべき")
	}

	calm := panicDaily(now, 40)
	last := &calm[len(calm)-1]
	last.Open, last.High, last.Low, last.Close, last.Volume = 1000, 1005, 995, 1000, 1000
	if IntradayHotCandidate("7203", calm, now) {
		t.Fatal("平常の銘柄が hot になっている")
	}

	// 当日の forming バーだけがパニック → 前日確定分では平常なので hot ではない。
	withToday := append(append([]market.Candle{}, calm...), market.Candle{
		Symbol: "7203", OpenTime: time.Date(2026, 7, 31, 0, 0, 0, 0, loc),
		Interval: 24 * time.Hour, Open: 940, High: 950, Low: 855, Close: 860, Volume: 5000,
	})
	if IntradayHotCandidate("7203", withToday, now) {
		t.Fatal("当日バーのパニックで hot になっている(前日確定分だけで判定すべき)")
	}
}

func TestBNFIntraday_EntersOnPrevDayPanicPlusIntradayReversal(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, jstZone) // trade day, mid-morning JST
	daily := panicDaily(now, 40)
	m5 := fiveMin(now, 4, true)

	sig := BNFIntradayReversion{}.Evaluate(bnfiInput(now, daily, m5, 848))
	if !sig.IsEntry() {
		t.Fatalf("expected entry, got %s (reason %s)", sig.Decision, sig.Reason)
	}
	if sig.Side != order.SideBuy {
		t.Fatalf("side = %s, want buy (BNF is long-only)", sig.Side)
	}
	if sig.HoldingMode != order.HoldingIntraday {
		t.Fatalf("holding mode = %q, want intraday (day-trade: no overnight)", sig.HoldingMode)
	}
	if sig.TakeProfitJPY <= 0 || sig.StopLossJPY <= 0 {
		t.Fatalf("TP/SL ticks must both be set (broker-side OCO): tp=%v sl=%v", sig.TakeProfitJPY, sig.StopLossJPY)
	}
	// SL sits just under today's low: entry 848, today's low from the 5m bars.
	tick := market.TickSize(848)
	todayLow := 0.0
	for i, c := range m5 {
		if i == 0 || c.Low < todayLow {
			todayLow = c.Low
		}
	}
	slPrice := 848 - sig.StopLossJPY*tick
	if slPrice >= todayLow {
		t.Fatalf("SL %v must be below today's low %v", slPrice, todayLow)
	}
	if sig.MaxHoldMinutes <= 0 {
		t.Fatalf("intraday needs a positive MaxHold (clamped to force-flat by the gate)")
	}
}

func TestBNFIntraday_ToleratesFormingDailyBar(t *testing.T) {
	// live の日足フィードは当日の形成中バーを含みうる。ゲートは前日の確定バーで判定すること。
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, jstZone)
	daily := panicDaily(now, 40)
	forming := market.Candle{
		Symbol: "7203", OpenTime: now.Truncate(24 * time.Hour), Interval: 24 * time.Hour,
		Open: 850, High: 856, Low: 845, Close: 848, Volume: 900, // calm partial bar
	}
	daily = append(daily, forming)

	sig := BNFIntradayReversion{}.Evaluate(bnfiInput(now, daily, fiveMin(now, 4, true), 848))
	if !sig.IsEntry() {
		t.Fatalf("forming daily bar must not mask yesterday's panic: got %s (reason %s)", sig.Decision, sig.Reason)
	}
}

func TestBNFIntraday_NoTradeCases(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, jstZone)
	panic40 := panicDaily(now, 40)

	calm := panicDaily(now, 40)
	last := calm[len(calm)-1]
	last.Open, last.High, last.Low, last.Close, last.Volume = 1000, 1005, 995, 1000, 1000
	calm[len(calm)-1] = last

	cases := []struct {
		name   string
		in     EvalInput
		reason string
	}{
		{"no panic yesterday", bnfiInput(now, calm, fiveMin(now, 4, true), 998), "no_prev_day_panic"},
		{"no intraday reversal yet", bnfiInput(now, panic40, fiveMin(now, 4, false), 848), "no_intraday_reversal"},
		{"too few completed 5m bars", bnfiInput(now, panic40, fiveMin(now, 2, true), 848), "insufficient_intraday_bars"},
		{"price already back above the 25-MA (no room)", bnfiInput(now, panic40, fiveMin(now, 4, true), 1010), "no_reversion_room"},
	}
	for _, tc := range cases {
		sig := BNFIntradayReversion{}.Evaluate(tc.in)
		if sig.IsEntry() {
			t.Errorf("%s: expected no_trade, got entry", tc.name)
			continue
		}
		if sig.Reason != tc.reason {
			t.Errorf("%s: reason = %q, want %q", tc.name, sig.Reason, tc.reason)
		}
	}
}

func TestBNFIntraday_ScreenerFlagsPanicDay(t *testing.T) {
	// screener は日足で走る(夕方スキャン): 最終確定バーのパニック = 「翌場を日計りする」。
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, jstZone)
	c := BNFIntradayReversion{}.Screen("7203", panicDaily(now, 40))
	if !c.Triggered {
		t.Fatalf("screener must trigger on a panic last bar: %+v", c)
	}
	if c.Strategy != config.StrategyBNFIntradayReversion {
		t.Fatalf("strategy = %s, want bnf_intraday_reversion", c.Strategy)
	}
}

// ---- bnf_intraday_reversion_trail -------------------------------
//
// 日中版の兄弟アーム。**入口は capped と同一の関数を共有**し、出口だけ
// 「TP(= 25 日線への戻りの 30% = 距離 A)を ratchet(arm = A / giveback = 1.5×A)へ
// 置き換える」。1.5 は多日 trail の giveback ÷ arm の比を借りたもので、新しい数字ではない。

func bnfiTrailInput(now time.Time, daily, m5 []market.Candle, last float64) EvalInput {
	in := bnfiInput(now, daily, m5, last)
	in.Config.StrategyName = config.StrategyBNFIntradayReversionTrail
	return in
}

// 🛑 入口判定が capped と 1 ビットも違わない(day2 / stabilized の同型テストと同じ作法)。
func TestBNFIntradayTrail_EntersExactlyWhenCappedDoes(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, jstZone)
	panic40 := panicDaily(now, 40)
	calm := panicDaily(now, 40)
	last := calm[len(calm)-1]
	last.Open, last.High, last.Low, last.Close, last.Volume = 1000, 1005, 995, 1000, 1000
	calm[len(calm)-1] = last

	cases := []struct {
		name       string
		daily, m5  []market.Candle
		price      float64
		wantEntry  bool
		wantReason string
	}{
		{"entry", panic40, fiveMin(now, 4, true), 848, true, "bnfi_panic_bounce"},
		{"no panic yesterday", calm, fiveMin(now, 4, true), 998, false, "no_prev_day_panic"},
		{"no intraday reversal yet", panic40, fiveMin(now, 4, false), 848, false, "no_intraday_reversal"},
		{"too few completed 5m bars", panic40, fiveMin(now, 2, true), 848, false, "insufficient_intraday_bars"},
		{"no reversion room", panic40, fiveMin(now, 4, true), 1010, false, "no_reversion_room"},
		{"no price", panic40, fiveMin(now, 4, true), 0, false, "no_price"},
	}
	for _, tc := range cases {
		capped := BNFIntradayReversion{}.Evaluate(bnfiInput(now, tc.daily, tc.m5, tc.price))
		trail := BNFIntradayReversionTrail{}.Evaluate(bnfiTrailInput(now, tc.daily, tc.m5, tc.price))
		if capped.IsEntry() != tc.wantEntry || trail.IsEntry() != tc.wantEntry {
			t.Fatalf("%s: entry capped=%v trail=%v want %v (reasons %q / %q)",
				tc.name, capped.IsEntry(), trail.IsEntry(), tc.wantEntry, capped.Reason, trail.Reason)
		}
		if capped.Reason != tc.wantReason || trail.Reason != tc.wantReason {
			t.Fatalf("%s: reason capped=%q trail=%q want %q", tc.name, capped.Reason, trail.Reason, tc.wantReason)
		}
		if trail.StrategyName != config.StrategyBNFIntradayReversionTrail {
			t.Fatalf("%s: strategy name = %q", tc.name, trail.StrategyName)
		}
		// 出口以外(建値・側・SL・MaxHold・HoldingMode)は同一。
		capped.TakeProfitJPY, capped.RatchetArmJPY, capped.RatchetGivebackJPY = 0, 0, 0
		trail.TakeProfitJPY, trail.RatchetArmJPY, trail.RatchetGivebackJPY = 0, 0, 0
		capped.StrategyName, trail.StrategyName = "", ""
		capped.CreatedAt, trail.CreatedAt = time.Time{}, time.Time{}
		if capped != trail {
			t.Fatalf("%s: signal differs beyond the exit:\n capped=%+v\n  trail=%+v", tc.name, capped, trail)
		}
	}
}

// 表: TP なし / SL 同じ / arm = A / giveback = 1.5×A / 14:50 強制(MaxHold 同じ)/ intraday。
func TestBNFIntradayTrail_ReplacesTheTPWithARatchetOfTheSameDistance(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, jstZone)
	daily, m5 := panicDaily(now, 40), fiveMin(now, 4, true)
	capped := BNFIntradayReversion{}.Evaluate(bnfiInput(now, daily, m5, 848))
	trail := BNFIntradayReversionTrail{}.Evaluate(bnfiTrailInput(now, daily, m5, 848))
	if !capped.IsEntry() || !trail.IsEntry() {
		t.Fatalf("precondition: both enter (capped %q / trail %q)", capped.Reason, trail.Reason)
	}
	a := capped.TakeProfitJPY
	if a <= 0 {
		t.Fatalf("precondition: capped TP (distance A) must be positive, got %v", a)
	}
	if trail.TakeProfitJPY != 0 {
		t.Errorf("trail TP = %v, want 0 (uncapped)", trail.TakeProfitJPY)
	}
	if trail.RatchetArmJPY != a {
		t.Errorf("ratchet arm = %v, want A = %v", trail.RatchetArmJPY, a)
	}
	if want := a * (bnfTrailGiveATR / bnfTrailArmATR); trail.RatchetGivebackJPY != want {
		t.Errorf("ratchet giveback = %v, want 1.5×A = %v (borrowed ratio, not a new number)", trail.RatchetGivebackJPY, want)
	}
	if capped.RatchetArmJPY != 0 || capped.RatchetGivebackJPY != 0 {
		t.Errorf("capped arm must not carry a ratchet: %+v", capped)
	}
	if trail.StopLossJPY != capped.StopLossJPY {
		t.Errorf("SL = %v, want same as capped %v", trail.StopLossJPY, capped.StopLossJPY)
	}
	if trail.MaxHoldMinutes != capped.MaxHoldMinutes {
		t.Errorf("MaxHold = %d, want same as capped %d", trail.MaxHoldMinutes, capped.MaxHoldMinutes)
	}
	if trail.HoldingMode != order.HoldingIntraday {
		t.Errorf("holding mode = %q, want intraday", trail.HoldingMode)
	}
}

// Screen は capped に委譲してラベルだけ張り替える(独自実装するとスクリーンと入口がズレる)。
func TestBNFIntradayTrail_ScreenDelegatesToCapped(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, jstZone)
	d := panicDaily(now, 40)
	base := BNFIntradayReversion{}.Screen("7203", d)
	tr := BNFIntradayReversionTrail{}.Screen("7203", d)
	if tr.Strategy != config.StrategyBNFIntradayReversionTrail {
		t.Fatalf("label = %q", tr.Strategy)
	}
	if tr.Triggered != base.Triggered || tr.Score != base.Score || !base.Triggered {
		t.Fatalf("screen must mirror capped: base=%+v trail=%+v", base, tr)
	}
	if tr.Detail == base.Detail {
		t.Errorf("Detail が同一 — 出口の違いが読めない: %q", tr.Detail)
	}
	if got := EntryArmOf(config.StrategyBNFIntradayReversionTrail); got != config.StrategyBNFIntradayReversion {
		t.Fatalf("EntryArmOf = %q, want bnf_intraday_reversion", got)
	}
}
