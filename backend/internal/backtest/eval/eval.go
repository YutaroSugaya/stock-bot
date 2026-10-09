// Package eval is the CODE-ENFORCED edge screen. It exists in code, not in
// hand-assembled jq pipelines, so the discipline cannot be re-sliced by an
// analyst after seeing results — the failure behind an earlier project's "fooled 4x" lesson.
// It NEVER emits "promote": the strongest verdict is screen_pass, because a real
// promotion additionally needs benchmark-excess and a one-shot holdout burn that
// live elsewhere.
package eval

import (
	"math"
	"sort"
	"time"

	"stockbot/backend/internal/backtest"
	"stockbot/backend/internal/backtest/judge"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
)

const holdoutRegime = -2 // closed on/after Config.HoldoutStart (excluded unless BurnHoldout)

var jst = clock.JST

// Config parameterises the screen. Zero values get methodology defaults.
type Config struct {
	HoldoutStart  time.Time   // trades closed >= this are reserved (excluded) unless BurnHoldout
	RegimeBounds  []time.Time // ascending in-sample cut points; N bounds => N+1 regimes
	RegimeNames   []string    // optional labels (len == regimes); defaults to R0..Rk
	BurnHoldout   bool        // evaluate the holdout window instead of the in-sample window (one-shot, recorded by caller)
	FloorMultiple float64     // require net mean >= cost_floor * this (default 1.5)
	MinRegimesPos int         // require >= this many regimes net-positive (default 2)
	Resamples     int         // bootstrap resamples (default 10000)
	Confidence    float64     // FAMILY-WISE CI confidence (default 0.95)
	Seed          int64       // bootstrap seed (default 42)
	Bench         *Bench      // optional market benchmark; when set, screen also requires excess CI_lo>0 (long-β kill)

	// TestCount is how many hypotheses the whole in-sample family has tested. The
	// day-block CI is widened by Bonferroni-Šidák (per-test confidence =
	// Confidence^(1/T)) so running many candidates cannot manufacture a false
	// winner. 0/1 → no widening.
	TestCount int
	// MaxTestCount is the hard test-budget cap (default 12). Exceeding it forces a
	// reject: the in-sample window is burned and a fresh holdout is required.
	MaxTestCount int
}

// Bench is a market benchmark close series (e.g. TOPIX 1306.T) for the
// benchmark-excess gate: a long-biased candidate must beat buy-and-hold of the
// market over each trade's own holding window, else it is just long-beta.
// Dates are JST midnight, ascending.
type Bench struct {
	dates  []time.Time
	closes []float64
}

// NewBench accepts parallel date/close slices in any order.
func NewBench(dates []time.Time, closes []float64) *Bench {
	type dc struct {
		d time.Time
		c float64
	}
	xs := make([]dc, 0, len(dates))
	for i := range dates {
		if i < len(closes) {
			xs = append(xs, dc{dateOnly(dates[i]), closes[i]})
		}
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i].d.Before(xs[j].d) })
	b := &Bench{}
	for _, x := range xs {
		b.dates = append(b.dates, x.d)
		b.closes = append(b.closes, x.c)
	}
	return b
}

func dateOnly(t time.Time) time.Time {
	j := t.In(jst)
	return time.Date(j.Year(), j.Month(), j.Day(), 0, 0, 0, 0, jst)
}

func (b *Bench) closeAtOrBefore(t time.Time) (float64, bool) {
	d := dateOnly(t)
	i := sort.Search(len(b.dates), func(i int) bool { return b.dates[i].After(d) })
	if i == 0 {
		return 0, false
	}
	return b.closes[i-1], true
}

// ret is the benchmark buy&hold return over [open, close].
func (b *Bench) ret(open, closed time.Time) (float64, bool) {
	o, ok1 := b.closeAtOrBefore(open)
	c, ok2 := b.closeAtOrBefore(closed)
	if !ok1 || !ok2 || o <= 0 {
		return 0, false
	}
	return c/o - 1, true
}

func (c *Config) defaults() {
	if c.FloorMultiple <= 0 {
		c.FloorMultiple = 1.5
	}
	if c.MinRegimesPos <= 0 {
		c.MinRegimesPos = 2
	}
	if c.Resamples <= 0 {
		c.Resamples = 10000
	}
	if c.Confidence <= 0 || c.Confidence >= 1 {
		c.Confidence = 0.95
	}
	if c.Seed == 0 {
		c.Seed = 42
	}
	if c.MaxTestCount <= 0 {
		c.MaxTestCount = 12
	}
}

// effConfidence widens Confidence to a per-test level via Bonferroni-Šidák
// (Confidence^(1/T)) so a T-hypothesis family keeps its family-wise level.
func (c *Config) effConfidence() float64 {
	t := c.TestCount
	if t <= 1 {
		return c.Confidence
	}
	return math.Pow(c.Confidence, 1.0/float64(t))
}

type Sample struct {
	Net      float64 // net per ¥1M notional
	Gross    float64 // gross per ¥1M notional
	BenchRet float64 // benchmark buy&hold over the same window, per ¥1M notional (0 if no bench)
	HasBench bool
	Day      string // JST close date "2006-01-02" (the bootstrap block key)
	Regime   int    // 0..k for in-sample regimes, holdoutRegime for the reserved window
	Side     order.Side
	Symbol   string
}

func regimeIndex(t time.Time, bounds []time.Time, holdoutStart time.Time) int {
	if !holdoutStart.IsZero() && !t.Before(holdoutStart) {
		return holdoutRegime
	}
	idx := 0
	for _, b := range bounds {
		if !t.Before(b) {
			idx++
		} else {
			break
		}
	}
	return idx
}

func BuildSamples(results []backtest.Result, cfg Config) []Sample {
	var out []Sample
	for _, r := range results {
		for _, t := range r.Trades {
			s := Sample{
				Net:    position.Per1MNotional(t.NetJPY, t.EntryPrice, t.Quantity),
				Gross:  position.Per1MNotional(t.GrossJPY, t.EntryPrice, t.Quantity),
				Day:    t.ClosedAt.In(jst).Format("2006-01-02"),
				Regime: regimeIndex(t.ClosedAt, cfg.RegimeBounds, cfg.HoldoutStart),
				Side:   t.Side,
				Symbol: r.Symbol,
			}
			if cfg.Bench != nil {
				if br, ok := cfg.Bench.ret(t.OpenedAt, t.ClosedAt); ok {
					// Long benefits from a market rise, short from a fall: sign the
					// benchmark leg by side so excess = own move beyond market beta.
					if t.Side == order.SideSell {
						br = -br
					}
					s.BenchRet = br * 1e6
					s.HasBench = true
				}
			}
			out = append(out, s)
		}
	}
	return out
}

type RegimeStat struct {
	Name    string  `json:"name"`
	N       int     `json:"n"`
	NetMean float64 `json:"net_mean_per_1m"`
	NetPF   float64 `json:"net_pf"`
}

// Result carries the verdict and its supporting statistics. All monetary figures
// are JPY-per-¥1,000,000-notional (price-level neutral).
type Result struct {
	Window          string       `json:"window"` // "in_sample" or "holdout"
	N               int          `json:"n"`
	UniverseN       int          `json:"universe_n"`
	NetMean         float64      `json:"net_mean_per_1m"`
	NetCILo         float64      `json:"net_ci_lo"`
	NetCIHi         float64      `json:"net_ci_hi"`
	NetPF           float64      `json:"net_pf"`
	GrossMean       float64      `json:"gross_mean_per_1m"`
	GrossPF         float64      `json:"gross_pf"`
	CostFloor       float64      `json:"cost_floor_per_1m"`
	BenchUsed       bool         `json:"bench_used"`
	BenchExcessMean float64      `json:"bench_excess_mean_per_1m"`
	BenchExcessCILo float64      `json:"bench_excess_ci_lo"`
	BenchExcessCIHi float64      `json:"bench_excess_ci_hi"`
	Regimes         []RegimeStat `json:"regimes"`
	RegimesPositive int          `json:"regimes_positive"`
	HoldoutReserved int          `json:"holdout_reserved_trades"` // excluded, not evaluated
	Verdict         string       `json:"verdict"`                 // reject | continue | screen_pass
	Reasons         []string     `json:"reasons"`
}

const (
	VerdictReject     = "reject"
	VerdictContinue   = "continue"
	VerdictScreenPass = "screen_pass" // passes the in-sample screen; STILL needs benchmark-excess + holdout burn (not done here)
)

func Evaluate(results []backtest.Result, cfg Config) Result {
	cfg.defaults()
	samples := BuildSamples(results, cfg)

	wantHoldout := cfg.BurnHoldout
	var used []Sample
	holdoutReserved := 0
	for _, s := range samples {
		isHoldout := s.Regime == holdoutRegime
		if isHoldout {
			if wantHoldout {
				used = append(used, s)
			} else {
				holdoutReserved++
			}
			continue
		}
		if !wantHoldout {
			used = append(used, s)
		}
	}

	res := Result{Window: "in_sample", HoldoutReserved: holdoutReserved}
	if wantHoldout {
		res.Window = "holdout"
	}
	res.N = len(used)
	if res.N == 0 {
		res.Verdict = VerdictContinue
		res.Reasons = []string{"no_trades"}
		return res
	}

	netVals := pick(used, func(s Sample) float64 { return s.Net })
	grossVals := pick(used, func(s Sample) float64 { return s.Gross })
	costVals := make([]float64, len(used))
	for i, s := range used {
		costVals[i] = s.Gross - s.Net
	}
	res.NetMean = judge.Mean(netVals)
	res.GrossMean = judge.Mean(grossVals)
	res.NetPF = judge.ProfitFactor(netVals)
	res.GrossPF = judge.ProfitFactor(grossVals)
	res.CostFloor = judge.Mean(costVals)
	res.UniverseN = distinctSymbols(used)
	res.NetCILo, res.NetCIHi = dayBlockCI(used, func(s Sample) float64 { return s.Net }, cfg)

	// Long-β kill: own gross move minus the market's over the same window. A real
	// edge beats buy&hold-the-market; long-beta does not.
	if cfg.Bench != nil {
		var ex []Sample
		for _, s := range used {
			if s.HasBench {
				ex = append(ex, s)
			}
		}
		if len(ex) > 0 {
			res.BenchUsed = true
			pickEx := func(s Sample) float64 { return s.Gross - s.BenchRet }
			res.BenchExcessMean = judge.Mean(pick(ex, pickEx))
			res.BenchExcessCILo, res.BenchExcessCIHi = dayBlockCI(ex, pickEx, cfg)
		}
	}

	if !wantHoldout {
		byReg := map[int][]float64{}
		for _, s := range used {
			byReg[s.Regime] = append(byReg[s.Regime], s.Net)
		}
		nReg := len(cfg.RegimeBounds) + 1
		for i := 0; i < nReg; i++ {
			vals := byReg[i]
			name := regimeName(cfg, i)
			st := RegimeStat{Name: name, N: len(vals)}
			if len(vals) > 0 { // 空のレジームは N=0・平均 0 で出す(NaN は JSON に出せない)
				st.NetMean, st.NetPF = judge.Mean(vals), judge.ProfitFactor(vals)
			}
			res.Regimes = append(res.Regimes, st)
			if st.N > 0 && st.NetMean > 0 {
				res.RegimesPositive++
			}
		}
	}

	res.Verdict, res.Reasons = decide(res, cfg, wantHoldout)
	return res
}

func decide(r Result, cfg Config, holdout bool) (string, []string) {
	var reasons []string
	// Once the family exceeds the test-budget cap the in-sample window is burned and
	// no candidate may screen_pass on it. Enforced in code so it is not a convention
	// an analyst can re-slice.
	if !holdout && cfg.MaxTestCount > 0 && cfg.TestCount > cfg.MaxTestCount {
		return VerdictReject, append(reasons, "test_budget_exceeded_hard_cap")
	}
	if r.N < 100 {
		return VerdictContinue, append(reasons, "insufficient_n_below_100")
	}
	if r.UniverseN < 3 {
		return VerdictContinue, append(reasons, "universe_too_narrow_below_3")
	}
	if r.NetCIHi < 0 {
		return VerdictReject, append(reasons, "day_block_ci_upper_negative")
	}
	if !holdout && r.RegimesPositive < cfg.MinRegimesPos {
		return VerdictContinue, append(reasons, "single_regime_mirage_regimes_positive_below_min")
	}
	if r.NetMean < r.CostFloor*cfg.FloorMultiple {
		return VerdictContinue, append(reasons, "net_below_cost_floor_multiple")
	}
	if r.BenchUsed && r.BenchExcessCILo <= 0 {
		return VerdictContinue, append(reasons, "no_excess_over_benchmark_long_beta")
	}
	if r.NetCILo > 0 && r.NetPF >= 1.1 {
		// Deliberately NOT "promote": still needs benchmark-excess (long-beta kill)
		// and a one-shot holdout burn, neither performed here.
		return VerdictScreenPass, append(reasons, "passes_in_sample_screen_pending_benchmark_excess_and_holdout")
	}
	return VerdictContinue, append(reasons, "day_block_ci_lo_not_positive_or_pf_below_1.1")
}

// dayBlockCI resamples whole CALENDAR DAYS with replacement so trades clustered
// on the same market-wide move stay together(judge.BootstrapMeanCIDayBlock そのもの)。
// eval が持つのは検定族の補正だけ: confidence を Bonferroni-Šidák で広げて渡す。
func dayBlockCI(samples []Sample, pickFn func(Sample) float64, cfg Config) (lo, hi float64) {
	days := make([]string, len(samples))
	for i, s := range samples {
		days[i] = s.Day
	}
	lo, hi, err := judge.BootstrapMeanCIDayBlock(pick(samples, pickFn), days, cfg.Resamples, cfg.effConfidence(), cfg.Seed)
	if err != nil {
		return 0, 0 // 空の標本(呼び手は N>0 のときだけ呼ぶ)
	}
	return lo, hi
}

func pick(ss []Sample, f func(Sample) float64) []float64 {
	out := make([]float64, len(ss))
	for i, s := range ss {
		out[i] = f(s)
	}
	return out
}

func distinctSymbols(ss []Sample) int {
	seen := map[string]struct{}{}
	for _, s := range ss {
		seen[s.Symbol] = struct{}{}
	}
	return len(seen)
}

func regimeName(cfg Config, i int) string {
	if i < len(cfg.RegimeNames) {
		return cfg.RegimeNames[i]
	}
	return "R" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
