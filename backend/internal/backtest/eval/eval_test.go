package eval

import (
	"testing"
	"time"

	"stockbot/backend/internal/backtest"
	"stockbot/backend/internal/backtest/judge"
	"stockbot/backend/internal/domain/order"
)

func d(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, jst)
	if err != nil {
		panic(err)
	}
	return t
}

func cfg() Config {
	return Config{
		HoldoutStart: d("2024-01-01"),
		RegimeBounds: []time.Time{d("2013-01-01"), d("2020-01-01")},
		RegimeNames:  []string{"2008-2012", "2013-2019", "2020-2023"},
	}
}

// Each trade closes on a distinct day so the day-block bootstrap has full
// resolution; entry 1000 × qty 100 makes net-per-1M == net*10.
func mkResult(symbol string, day0 time.Time, nets []float64) backtest.Result {
	r := backtest.Result{Symbol: symbol}
	for i, n := range nets {
		ct := day0.AddDate(0, 0, i)
		r.Trades = append(r.Trades, backtest.Trade{
			Symbol: symbol, Side: order.SideBuy, EntryPrice: 1000, Quantity: 100,
			OpenedAt: ct.AddDate(0, 0, -1), ClosedAt: ct, GrossJPY: n, NetJPY: n,
		})
	}
	return r
}

func rep(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// eval の CI は judge の day-block bootstrap そのもの(Šidák で補正した confidence を渡すだけ)。
// 2 実装に割ると、同じ標本に edge-eval と edge-judge が別の CI を出す。
func TestEvaluate_NetCIIsJudgesDayBlockCI(t *testing.T) {
	c := cfg()
	c.TestCount = 3
	varied := make([]float64, 60)
	for i := range varied {
		varied[i] = float64(i%7*40 - 90)
	}
	results := []backtest.Result{
		mkResult("A", d("2009-01-05"), varied[:20]),
		mkResult("B", d("2015-01-06"), varied[20:40]),
		mkResult("C", d("2021-01-04"), varied[40:]),
	}
	res := Evaluate(results, c)
	c.defaults()
	var vals []float64
	var days []string
	for _, s := range BuildSamples(results, c) {
		vals = append(vals, s.Net)
		days = append(days, s.Day)
	}
	lo, hi, err := judge.BootstrapMeanCIDayBlock(vals, days, c.Resamples, c.effConfidence(), c.Seed)
	if err != nil {
		t.Fatal(err)
	}
	if res.NetCILo != lo || res.NetCIHi != hi {
		t.Fatalf("eval CI [%v,%v] != judge CI [%v,%v]", res.NetCILo, res.NetCIHi, lo, hi)
	}
}

// 標本の無いレジームは件数 0・平均 0 のまま JSON に出る(NaN は encoding/json が落ちる)。
func TestEvaluate_EmptyRegimeHasZeroMean(t *testing.T) {
	results := []backtest.Result{mkResult("A", d("2009-01-05"), rep(50, 30))}
	res := Evaluate(results, cfg())
	if len(res.Regimes) != 3 {
		t.Fatalf("regimes = %d", len(res.Regimes))
	}
	for _, r := range res.Regimes[1:] {
		if r.N != 0 || r.NetMean != 0 || r.NetPF != 0 {
			t.Fatalf("空のレジーム = %+v, want N=0 mean=0 pf=0", r)
		}
	}
}

func TestRegimeIndex(t *testing.T) {
	c := cfg()
	cases := []struct {
		date string
		want int
	}{
		{"2009-06-01", 0}, {"2012-12-31", 0}, {"2013-01-01", 1},
		{"2019-12-31", 1}, {"2020-01-01", 2}, {"2023-12-31", 2},
		{"2024-01-02", holdoutRegime},
	}
	for _, tc := range cases {
		if got := regimeIndex(d(tc.date), c.RegimeBounds, c.HoldoutStart); got != tc.want {
			t.Fatalf("regime(%s) = %d, want %d", tc.date, got, tc.want)
		}
	}
}

func TestEvaluate_HoldoutReservedByDefault(t *testing.T) {
	c := cfg()
	results := []backtest.Result{
		mkResult("A", d("2010-01-04"), rep(100, 60)),
		mkResult("B", d("2015-01-05"), rep(100, 60)),
		mkResult("C", d("2024-02-01"), rep(100, 40)), // holdout — must NOT be evaluated
	}
	res := Evaluate(results, c)
	if res.HoldoutReserved != 40 {
		t.Fatalf("HoldoutReserved = %d, want 40", res.HoldoutReserved)
	}
	if res.N != 120 {
		t.Fatalf("in-sample N = %d, want 120 (holdout excluded)", res.N)
	}
	if res.Window != "in_sample" {
		t.Fatalf("window = %q", res.Window)
	}
}

func TestEvaluate_SingleRegimeMirage(t *testing.T) {
	c := cfg()
	results := []backtest.Result{
		mkResult("A", d("2009-01-05"), rep(-50, 40)), // R0 negative
		mkResult("B", d("2014-01-06"), rep(-50, 40)), // R1 negative
		mkResult("C", d("2021-01-04"), rep(200, 60)), // R2 strongly positive
	}
	res := Evaluate(results, c)
	if res.RegimesPositive != 1 {
		t.Fatalf("RegimesPositive = %d, want 1", res.RegimesPositive)
	}
	if res.Verdict != VerdictContinue {
		t.Fatalf("verdict = %q, want continue", res.Verdict)
	}
	if len(res.Reasons) == 0 || res.Reasons[0] != "single_regime_mirage_regimes_positive_below_min" {
		t.Fatalf("reasons = %v, want single_regime_mirage", res.Reasons)
	}
}

func TestEvaluate_RejectsOnNegativeCIUpper(t *testing.T) {
	c := cfg()
	results := []backtest.Result{
		mkResult("A", d("2009-01-05"), rep(-100, 50)),
		mkResult("B", d("2014-01-06"), rep(-100, 50)),
		mkResult("C", d("2021-01-04"), rep(-100, 50)),
	}
	res := Evaluate(results, c)
	if res.Verdict != VerdictReject {
		t.Fatalf("verdict = %q, want reject (CI upper < 0)", res.Verdict)
	}
}

func TestEvaluate_ScreenPass(t *testing.T) {
	c := cfg()
	results := []backtest.Result{
		mkResult("A", d("2009-01-05"), rep(100, 50)),
		mkResult("B", d("2015-01-06"), rep(100, 50)),
		mkResult("C", d("2021-01-04"), rep(100, 50)),
	}
	res := Evaluate(results, c)
	if res.RegimesPositive != 3 {
		t.Fatalf("RegimesPositive = %d, want 3", res.RegimesPositive)
	}
	if res.Verdict != VerdictScreenPass {
		t.Fatalf("verdict = %q, want screen_pass (reasons=%v)", res.Verdict, res.Reasons)
	}
	if res.NetCILo <= 0 {
		t.Fatalf("NetCILo = %v, want > 0", res.NetCILo)
	}
}

// The day-block CI must WIDEN with the family test count, so running many
// candidates cannot manufacture a false winner.
func TestEvaluate_SidakWidensCIWithTestCount(t *testing.T) {
	c := cfg()
	// Identical values collapse the CI to zero width, so vary the nets.
	varied := make([]float64, 150)
	for i := range varied {
		if i%2 == 0 {
			varied[i] = 50
		} else {
			varied[i] = 150
		}
	}
	results := []backtest.Result{
		mkResult("A", d("2009-01-05"), varied[:50]),
		mkResult("B", d("2015-01-06"), varied[50:100]),
		mkResult("C", d("2021-01-04"), varied[100:150]),
	}
	c.TestCount = 1
	r1 := Evaluate(results, c)
	c.TestCount = 12
	r12 := Evaluate(results, c)
	if !(r12.NetCILo < r1.NetCILo && r12.NetCIHi > r1.NetCIHi) {
		t.Fatalf("T=12 CI [%.2f,%.2f] must be WIDER than T=1 [%.2f,%.2f] (Šidák widening)",
			r12.NetCILo, r12.NetCIHi, r1.NetCILo, r1.NetCIHi)
	}
}

// Exceeding the test-budget hard cap burns the in-sample window: no candidate may
// screen_pass on it.
func TestEvaluate_TestBudgetHardCapRejects(t *testing.T) {
	c := cfg()
	results := []backtest.Result{
		mkResult("A", d("2009-01-05"), rep(100, 50)),
		mkResult("B", d("2015-01-06"), rep(100, 50)),
		mkResult("C", d("2021-01-04"), rep(100, 50)),
	}
	c.TestCount = 13 // over the cap
	if res := Evaluate(results, c); res.Verdict != VerdictReject || res.Reasons[0] != "test_budget_exceeded_hard_cap" {
		t.Fatalf("T=13 over cap must reject for budget, got verdict=%q reasons=%v", res.Verdict, res.Reasons)
	}
	c.TestCount = 12 // exactly at the cap → still allowed to pass
	if res := Evaluate(results, c); res.Verdict != VerdictScreenPass {
		t.Fatalf("T=12 (== cap) must not be budget-rejected, got %q (%v)", res.Verdict, res.Reasons)
	}
}

func TestEvaluate_BurnHoldoutEvaluatesReservedWindow(t *testing.T) {
	c := cfg()
	c.BurnHoldout = true
	results := []backtest.Result{
		mkResult("A", d("2010-01-04"), rep(100, 60)),  // in-sample, ignored when burning
		mkResult("C", d("2024-02-01"), rep(100, 120)), // holdout, evaluated
	}
	res := Evaluate(results, c)
	if res.Window != "holdout" {
		t.Fatalf("window = %q, want holdout", res.Window)
	}
	if res.N != 120 {
		t.Fatalf("holdout N = %d, want 120", res.N)
	}
}
