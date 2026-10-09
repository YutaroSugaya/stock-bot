// Command edge-eval is the CODE-ENFORCED edge screen (EDGE_METHODOLOGY.md). It
// reads per-symbol backtest Result JSONs (the -json output of cmd/backtest),
// pools and normalises them to JPY-per-¥1,000,000-notional, partitions the
// in-sample window into regimes, and reports a day-block-bootstrap verdict. It
// reserves the holdout window (>= -holdout-start) and REFUSES to read it unless
// -burn-holdout is given (a one-shot, recorded action). It never prints
// "promote": a screen_pass still owes benchmark-excess + a holdout burn.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"stockbot/backend/internal/backtest"
	"stockbot/backend/internal/backtest/eval"
	"stockbot/backend/internal/domain/clock"
)

func main() {
	var (
		glob        = flag.String("glob", "", "glob of per-symbol backtest Result JSON files (cmd/backtest -json)")
		holdout     = flag.String("holdout-start", "2024-01-01", "JST date; trades closed >= this are reserved")
		regimeStr   = flag.String("regime-bounds", "2013-01-01,2020-01-01", "ascending JST cut points; N => N+1 regimes")
		floorMult   = flag.Float64("floor-multiple", 1.5, "require net mean >= cost_floor * this")
		minRegimes  = flag.Int("min-regimes-positive", 2, "require >= this many in-sample regimes net-positive")
		burnHoldout = flag.Bool("burn-holdout", false, "DANGER: evaluate the reserved holdout window (one-shot, burns it)")
		benchPath   = flag.String("benchmark", "", "benchmark CSV (DateJST;O;H;L;C;V, e.g. TOPIX 1306) for the excess gate")
		label       = flag.String("label", "", "candidate label (recorded in output)")
		testCount   = flag.Int("test-count", 0, "family test count T (検定の台帳); widens the CI by Bonferroni-Šidák and enforces the budget cap")
		maxTests    = flag.Int("max-test-count", 12, "hard test-budget cap (EDGE_METHODOLOGY §2); T beyond this forces a reject")
	)
	flag.Parse()
	if *glob == "" {
		fmt.Fprintln(os.Stderr, "usage: edge-eval -glob '/path/*.json' [flags]")
		os.Exit(2)
	}
	files, err := filepath.Glob(*glob)
	if err != nil || len(files) == 0 {
		fatal(fmt.Errorf("no files match %q (err=%v)", *glob, err))
	}

	var results []backtest.Result
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			fatal(err)
		}
		var r backtest.Result
		if err := json.Unmarshal(b, &r); err != nil {
			fatal(fmt.Errorf("parse %s: %w", f, err))
		}
		results = append(results, r)
	}

	cfg := eval.Config{
		HoldoutStart:  parseDate(*holdout),
		RegimeBounds:  parseDates(*regimeStr),
		FloorMultiple: *floorMult,
		MinRegimesPos: *minRegimes,
		BurnHoldout:   *burnHoldout,
		TestCount:     *testCount,
		MaxTestCount:  *maxTests,
	}
	if *benchPath != "" {
		cfg.Bench = loadBench(*benchPath)
	}
	res := eval.Evaluate(results, cfg)

	fmt.Fprintf(os.Stderr, "── edge-eval %s [%s] ───────────────\n", *label, res.Window)
	if *burnHoldout {
		fmt.Fprintf(os.Stderr, "⚠ HOLDOUT BURNED for %q — this window is now spent; reject permanently if the edge collapses.\n", *label)
	}
	fmt.Fprintf(os.Stderr, "N=%d universe=%d  net=%.1f/1M  CI(day-block)=[%.1f, %.1f]  netPF=%.3f  grossPF=%.3f  floor=%.1f\n",
		res.N, res.UniverseN, res.NetMean, res.NetCILo, res.NetCIHi, res.NetPF, res.GrossPF, res.CostFloor)
	if res.BenchUsed {
		fmt.Fprintf(os.Stderr, "bench-excess (gross − TOPIX, β kill): mean=%.1f/1M  CI(day-block)=[%.1f, %.1f]\n",
			res.BenchExcessMean, res.BenchExcessCILo, res.BenchExcessCIHi)
	}
	for _, rg := range res.Regimes {
		fmt.Fprintf(os.Stderr, "  regime %-12s N=%-5d net=%.1f/1M  PF=%.3f\n", rg.Name, rg.N, rg.NetMean, rg.NetPF)
	}
	if res.HoldoutReserved > 0 && !*burnHoldout {
		fmt.Fprintf(os.Stderr, "  (%d holdout trades reserved, untouched)\n", res.HoldoutReserved)
	}
	fmt.Fprintf(os.Stderr, "verdict: %s\n", res.Verdict)
	for _, r := range res.Reasons {
		fmt.Fprintf(os.Stderr, "  - %s\n", r)
	}
	if res.Verdict == eval.VerdictScreenPass {
		fmt.Fprintf(os.Stderr, "⚠ screen_pass は promote ではない。次: ベンチ超過(long-β殺し)+ holdout 1回 burn。最終 live は人間。\n")
	}
	fmt.Fprintf(os.Stderr, "──────────────────────────────────────\n")

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
}

// loadBench parses a benchmark CSV (DateJST;O;H;L;C;V) into an eval.Bench using
// the close column. Header and malformed rows are skipped.
func loadBench(path string) *eval.Bench {
	b, err := os.ReadFile(path)
	if err != nil {
		fatal(fmt.Errorf("read benchmark %q: %w", path, err))
	}
	var dates []time.Time
	var closes []float64
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "DateJST") {
			continue
		}
		f := strings.Split(line, ";")
		if len(f) < 5 {
			continue
		}
		d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(f[0]), clock.JST)
		if err != nil {
			continue
		}
		c, err := strconv.ParseFloat(strings.TrimSpace(f[4]), 64)
		if err != nil {
			continue
		}
		dates = append(dates, d)
		closes = append(closes, c)
	}
	if len(dates) == 0 {
		fatal(fmt.Errorf("benchmark %q had no usable rows", path))
	}
	return eval.NewBench(dates, closes)
}

func parseDate(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(s), clock.JST)
	if err != nil {
		fatal(fmt.Errorf("bad date %q: %w", s, err))
	}
	return t
}

func parseDates(s string) []time.Time {
	var out []time.Time
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, parseDate(p))
	}
	return out
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "edge-eval:", err)
	os.Exit(1)
}
