package judge

import (
	"encoding/json"
	"math"
	"testing"
)

func series(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// mix builds n values: `wins` of +w and the rest of -l.
func mix(n, wins int, w, l float64) []float64 {
	out := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		if i < wins {
			out = append(out, w)
		} else {
			out = append(out, -l)
		}
	}
	return out
}

func TestBootstrap_Deterministic(t *testing.T) {
	vals := mix(200, 120, 100, 80)
	lo1, hi1, err := BootstrapMeanCI(vals, 5000, 0.95, 42)
	if err != nil {
		t.Fatal(err)
	}
	lo2, hi2, _ := BootstrapMeanCI(vals, 5000, 0.95, 42)
	if lo1 != lo2 || hi1 != hi2 {
		t.Fatalf("bootstrap not deterministic: (%g,%g) vs (%g,%g)", lo1, hi1, lo2, hi2)
	}
	if !(lo1 <= Mean(vals) && Mean(vals) <= hi1) {
		t.Fatalf("mean %g not in CI [%g,%g]", Mean(vals), lo1, hi1)
	}
	if _, _, err := BootstrapMeanCI(nil, 100, 0.95, 1); err == nil {
		t.Fatal("empty sample should error")
	}
}

// clustered gives every trade on a day that day's outcome — 多銘柄 forward が実際
// に持つ日次クラスタ(市場全体が動いた日は全建玉が同じ向きに動く)の形。
func clustered(days, per int, dayVals []float64) (vals []float64, dayKeys []string) {
	for d := 0; d < days; d++ {
		key := "2026-07-" + pad2(d+1)
		for i := 0; i < per; i++ {
			vals = append(vals, dayVals[d%len(dayVals)])
			dayKeys = append(dayKeys, key)
		}
	}
	return vals, dayKeys
}

func pad2(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func TestBootstrapDayBlock_WiderThanTradeLevel(t *testing.T) {
	// 20 日 × 10 本、日ごとに全建玉が同じ結果 = 実効標本は 200 ではなく 20。
	vals, days := clustered(20, 10, []float64{300, -200, 150, -120, 80})
	tLo, tHi, err := BootstrapMeanCI(vals, 5000, 0.95, 42)
	if err != nil {
		t.Fatal(err)
	}
	dLo, dHi, err := BootstrapMeanCIDayBlock(vals, days, 5000, 0.95, 42)
	if err != nil {
		t.Fatal(err)
	}
	if (dHi - dLo) <= (tHi - tLo) {
		t.Fatalf("day-block CI must be wider than trade-level on clustered data: day=[%g,%g] w=%g trade=[%g,%g] w=%g",
			dLo, dHi, dHi-dLo, tLo, tHi, tHi-tLo)
	}
	if !(dLo <= Mean(vals) && Mean(vals) <= dHi) {
		t.Fatalf("mean %g outside day-block CI [%g,%g]", Mean(vals), dLo, dHi)
	}
}

func TestBootstrapDayBlock_Deterministic(t *testing.T) {
	vals, days := clustered(12, 8, []float64{120, -90, 40})
	lo1, hi1, err := BootstrapMeanCIDayBlock(vals, days, 3000, 0.95, 7)
	if err != nil {
		t.Fatal(err)
	}
	lo2, hi2, _ := BootstrapMeanCIDayBlock(vals, days, 3000, 0.95, 7)
	if lo1 != lo2 || hi1 != hi2 {
		t.Fatalf("day-block bootstrap not deterministic: (%g,%g) vs (%g,%g)", lo1, hi1, lo2, hi2)
	}
	if _, _, err := BootstrapMeanCIDayBlock(nil, nil, 100, 0.95, 1); err == nil {
		t.Fatal("empty sample should error")
	}
	if _, _, err := BootstrapMeanCIDayBlock(vals, days[:3], 100, 0.95, 1); err == nil {
		t.Fatal("length mismatch should error")
	}
}

func TestJudge_DaysLengthMismatchIsAnError(t *testing.T) {
	vals, days := clustered(20, 10, []float64{100, -50})
	if _, err := Judge(JudgeInput{Track: "B", NetPnLJPY: vals, Days: days[:5], UniverseN: 5}); err == nil {
		t.Fatal("days present but wrong length must be an error, not a silent fallback")
	}
}

func TestJudge_DaysAbsentFlagsAntiConservativeBootstrap(t *testing.T) {
	r, err := Judge(JudgeInput{Track: "B", NetPnLJPY: mix(150, 110, 100, 40), UniverseN: 8, CostFloorJPY: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !hasReason(r, "trade_level_bootstrap_anti_conservative") {
		t.Fatalf("missing days must be disclosed in reasons, got %v", r.Reasons)
	}
}

func TestJudge_DaysPresentUsesDayBlockCI(t *testing.T) {
	vals, days := clustered(20, 10, []float64{300, -200, 150, -120, 80})
	withDays, err := Judge(JudgeInput{Track: "B", NetPnLJPY: vals, Days: days, UniverseN: 8})
	if err != nil {
		t.Fatal(err)
	}
	withoutDays, err := Judge(JudgeInput{Track: "B", NetPnLJPY: vals, UniverseN: 8})
	if err != nil {
		t.Fatal(err)
	}
	if (withDays.CIHi - withDays.CILo) <= (withoutDays.CIHi - withoutDays.CILo) {
		t.Fatalf("Judge with days must use the wider day-block CI: %v vs %v", withDays, withoutDays)
	}
	if hasReason(withDays, "trade_level_bootstrap_anti_conservative") {
		t.Fatalf("day-block path must not carry the anti-conservative warning: %v", withDays.Reasons)
	}
}

func TestJudge_InsufficientN(t *testing.T) {
	r := mustJudge(t, JudgeInput{Track: "B", NetPnLJPY: mix(50, 40, 100, 50), UniverseN: 5})
	if r.Verdict != VerdictContinue {
		t.Fatalf("N=50 track B should be continue, got %s", r.Verdict)
	}
}

func mustJudge(t *testing.T, in JudgeInput) JudgeResult {
	t.Helper()
	r, err := Judge(in)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	return r
}

func TestJudge_RejectNegativeEdge(t *testing.T) {
	r := mustJudge(t, JudgeInput{Track: "B", NetPnLJPY: series(150, -50), UniverseN: 5})
	if r.Verdict != VerdictReject {
		t.Fatalf("consistent loss should reject, got %s (CIhi=%.2f)", r.Verdict, r.CIHi)
	}
}

func TestJudge_UniverseTooNarrow(t *testing.T) {
	r := mustJudge(t, JudgeInput{Track: "B", NetPnLJPY: mix(150, 110, 100, 40), UniverseN: 1, CostFloorJPY: 0})
	if r.Verdict != VerdictContinue || !hasReason(r, "universe_too_narrow") {
		t.Fatalf("single-symbol edge should be gated, got %s %v", r.Verdict, r.Reasons)
	}
}

func TestJudge_PromoteCandidate_TrackB(t *testing.T) {
	net := mix(150, 110, 100, 40) // strong positive
	oos := mix(80, 58, 100, 40)   // same-sign positive
	r := mustJudge(t, JudgeInput{
		Track: "B", NetPnLJPY: net, OOSNetPnLJPY: oos, CostFloorJPY: 5, FloorMultiple: 1.5, UniverseN: 8,
	})
	if r.Verdict != VerdictCandidate {
		t.Fatalf("strong edge should promote_candidate, got %s %v", r.Verdict, r.Reasons)
	}
	if math.IsNaN(r.OOSMean) || r.OOSMean <= 0 {
		t.Fatalf("OOS mean should be positive: %v", r.OOSMean)
	}
}

func TestJudge_TrackARequiresOOS(t *testing.T) {
	net := mix(400, 300, 100, 40) // strong positive, N>=300
	r := mustJudge(t, JudgeInput{Track: "A", NetPnLJPY: net, CostFloorJPY: 5, FloorMultiple: 1.5, UniverseN: 8})
	if r.Verdict == VerdictCandidate {
		t.Fatalf("track A without OOS must not promote, got %s", r.Verdict)
	}
	if !hasReason(r, "oos_absent_track_A_cannot_promote") {
		t.Fatalf("expected OOS-absent reason, got %v", r.Reasons)
	}
}

func TestJudge_BelowCostFloor(t *testing.T) {
	net := mix(150, 76, 10, 9) // 正だが cost floor を下回る
	r := mustJudge(t, JudgeInput{Track: "B", NetPnLJPY: net, CostFloorJPY: 50, FloorMultiple: 1.5, UniverseN: 8})
	if r.Verdict == VerdictCandidate {
		t.Fatalf("edge below cost floor must not promote, got %s (mean=%.2f)", r.Verdict, r.Mean)
	}
}

func hasReason(r JudgeResult, want string) bool {
	for _, s := range r.Reasons {
		if s == want || contains(s, want) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(sub) > 0 && indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// OOS / bench が無いときそれらは NaN で、encoding/json は NaN を**エラーにする** —
// cmd 側が err を捨てていたため、以前は機械可読な verdict 行は一度も出て
// いなかった。NaN は null にして必ずエンコードできることを固定する。
func TestJudgeResultMarshalsWithAbsentOOSAndBench(t *testing.T) {
	r := mustJudge(t, JudgeInput{Track: "B", NetPnLJPY: mix(150, 110, 100, 40), UniverseN: 8})
	if !math.IsNaN(r.OOSMean) || !math.IsNaN(r.BenchExcess) {
		t.Fatalf("前提: OOS/bench 不在は NaN のはず: %v / %v", r.OOSMean, r.BenchExcess)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("JudgeResult は必ず JSON にできること(NaN で落ちない): %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("再パース: %v", err)
	}
	if v, ok := back["oos_mean_jpy"]; !ok || v != nil {
		t.Fatalf("oos_mean_jpy = %v, want null(0 に化けさせない)", v)
	}
	if v, ok := back["bench_excess_jpy"]; !ok || v != nil {
		t.Fatalf("bench_excess_jpy = %v, want null", v)
	}
	if back["verdict"] != string(r.Verdict) || back["n"] != float64(r.N) {
		t.Fatalf("他のフィールドが落ちている: %v", back)
	}
}

// 値があるときは数値としてそのまま出る(null 化は NaN のときだけ)。
func TestJudgeResultMarshalsRealOOSMean(t *testing.T) {
	r := mustJudge(t, JudgeInput{Track: "B", NetPnLJPY: mix(150, 110, 100, 40),
		OOSNetPnLJPY: mix(80, 58, 100, 40), UniverseN: 8})
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back["oos_mean_jpy"] == nil {
		t.Fatalf("OOS があるのに null: %s", b)
	}
}

// レビュー指摘: 標本が **1日に集中**していると全リサンプル平均が同一値に
// なって **CI 幅がゼロ**に潰れ、`CILo > 0` のゲートが「平均 > 0」に退化する
// (continue が promote_candidate に化ける)。判定不能として扱い、黙って通さない。
func TestJudge_SingleDayClusterDoesNotCollapseCI(t *testing.T) {
	// 120 トレード・全て同一日・平均 +50 / PF 1.10 — 旧実装では CI=[50,50] で
	// promote_candidate になっていた。
	var vals []float64
	var days []string
	for i := 0; i < 120; i++ {
		if i%2 == 0 {
			vals = append(vals, 1100)
		} else {
			vals = append(vals, -1000)
		}
		days = append(days, "2026-08-07")
	}
	r, err := Judge(JudgeInput{Track: "B", NetPnLJPY: vals, Days: days, UniverseN: 40})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict == VerdictCandidate {
		t.Fatalf("1日クラスタで promote してはいけない: verdict=%s CI=[%g,%g]", r.Verdict, r.CILo, r.CIHi)
	}
	if !hasReason(r, "insufficient_day_blocks") {
		t.Fatalf("ブロック数不足を reasons に出すこと(黙って潰れた CI を出さない): %v", r.Reasons)
	}
	if r.DayBlocks != 1 {
		t.Fatalf("DayBlocks = %d, want 1(判定の実効標本を必ず見せる)", r.DayBlocks)
	}
}

// 十分な日数があれば従来どおり判定する(ゲートが常時 continue を返す壊れ方をしない)。
func TestJudge_EnoughDayBlocksStillJudges(t *testing.T) {
	vals, days := clustered(minDayBlocks, 6, []float64{300, -200, 150, -120, 80})
	r, err := Judge(JudgeInput{Track: "B", NetPnLJPY: vals, Days: days, UniverseN: 40})
	if err != nil {
		t.Fatal(err)
	}
	if hasReason(r, "insufficient_day_blocks") {
		t.Fatalf("%d 日あれば日数ゲートは当たらないこと: %v", minDayBlocks, r.Reasons)
	}
	if r.DayBlocks != minDayBlocks {
		t.Fatalf("DayBlocks = %d, want %d", r.DayBlocks, minDayBlocks)
	}
	if r.CIHi <= r.CILo {
		t.Fatalf("CI が潰れている: [%g,%g]", r.CILo, r.CIHi)
	}
}

// 🛑 空の標本の平均は **NaN**(定義されない)。0 を返すと「測って 0 だった」と読める
// (MarshalJSON が NaN を null に出すのと同じ規約)。件数 0 を 0 で表に出したい呼び手は
// 自分で件数を見る。
func TestMean_EmptyIsNaN(t *testing.T) {
	if got := Mean(nil); !math.IsNaN(got) {
		t.Fatalf("Mean(nil) = %v, want NaN", got)
	}
	if got := Mean([]float64{1, 2, 6}); got != 3 {
		t.Fatalf("Mean = %v, want 3", got)
	}
}

func TestProfitFactor(t *testing.T) {
	if got := ProfitFactor([]float64{30, -10, -5}); got != 2 {
		t.Fatalf("PF = %v, want 2", got)
	}
	if got := ProfitFactor(nil); got != 0 {
		t.Fatalf("空の PF = %v, want 0", got)
	}
	if got := ProfitFactor([]float64{5}); !math.IsInf(got, 1) {
		t.Fatalf("負けゼロの PF = %v, want +Inf", got)
	}
}
