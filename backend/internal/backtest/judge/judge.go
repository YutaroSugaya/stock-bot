package judge

import (
	"encoding/json"
	"fmt"
	"math"
)

type Verdict string

const (
	VerdictReject    Verdict = "reject"
	VerdictCandidate Verdict = "promote_candidate"
	VerdictContinue  Verdict = "continue"
)

type JudgeInput struct {
	Track     string    // "A" (intraday, N>=300) or "B" (multiday, N>=100)
	NetPnLJPY []float64 // in-sample / live net per trade
	// Days は NetPnLJPY と**同順同長**の JST 日付 ("2006-01-02")。在れば日クラスタを
	// 保つ day-block bootstrap、空ならトレード単位に落ちるが、その CI は楽観側に
	// 出るので reasons に必ず注記する。長さ不一致は黙って落とさずエラー。
	Days           []string
	OOSNetPnLJPY   []float64 // out-of-sample / holdout net per trade (optional)
	BenchNetPnLJPY []float64 // benchmark (buy&hold / index) net over the same bars (optional)
	CostFloorJPY   float64   // mean round-trip cost JPY/trade for this universe
	FloorMultiple  float64   // require mean >= CostFloorJPY * this (default 1.0; track A ~1.5)
	UniverseN      int       // distinct symbols contributing (嘘発見器 breadth gate)
	Resamples      int       // default 10000
	Seed           int64     // default 42
	Confidence     float64   // default 0.95
}

type JudgeResult struct {
	N           int      `json:"n"`
	Mean        float64  `json:"net_expectancy_jpy"`
	CILo        float64  `json:"ci_lo"`
	CIHi        float64  `json:"ci_hi"`
	NetPF       float64  `json:"net_pf"`
	OOSMean     float64  `json:"oos_mean_jpy"`
	BenchExcess float64  `json:"bench_excess_jpy"`
	Verdict     Verdict  `json:"verdict"`
	Reasons     []string `json:"reasons"`
	// DayBlocks(暦日数)が判定の実効 N。**トレード数ではない** — 多銘柄 forward は
	// トレードが日単位でクラスタするので、380 トレードでも独立な観測は営業日数しか
	// ない。0 = days が渡されなかった。
	DayBlocks int `json:"day_blocks"`
}

// minDayBlocks: d=1 だと復元抽出は毎回同じブロックを引くので全リサンプル平均が
// 同一値になり、CI 幅がゼロに潰れて `CILo > 0` が「平均 > 0」に退化する — CI を
// 保守的にするための実装が、この領域では**最大限に楽観側**へ倒れる。
// 20 未満は判定不能として continue に落とす。
const minDayBlocks = 20

// MarshalJSON emits NaN as null. OOSMean / BenchExcess は「その検査を走らせて
// いない」を NaN で表すが encoding/json は NaN をエラーにし、cmd/edge-judge が
// Encode の err を捨てると**機械可読な verdict 行が一度も
// 出ない**。0 に丸めると「OOS 平均ゼロ」と読めてしまうので null。
// 非有限は NaN だけではない: N=0 の Mean(空平均)と負けゼロの NetPF(+Inf)も落ちる。
func (r JudgeResult) MarshalJSON() ([]byte, error) {
	type alias JudgeResult // avoid recursion
	return json.Marshal(struct {
		alias
		Mean        *float64 `json:"net_expectancy_jpy"`
		NetPF       *float64 `json:"net_pf"`
		OOSMean     *float64 `json:"oos_mean_jpy"`
		BenchExcess *float64 `json:"bench_excess_jpy"`
	}{alias(r), finite(r.Mean), finite(r.NetPF), finite(r.OOSMean), finite(r.BenchExcess)})
}

// finite returns nil for NaN/±Inf so the field marshals to null: 0 に丸めると
// 「測って 0 だった」と読め、未実施 / 分母ゼロと区別がつかない。
func finite(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// Judge applies the pass/reject rules. The gate order below matters (first match
// wins): never promote on gross, never promote a single-symbol fluke, require OOS
// same-sign for track A.
//
// エラーを返すのは入力が壊れているときだけ(Days の長さ不一致)。判定不能を
// verdict に化けさせない — 黙って楽観 CI に落ちる経路を塞ぐための返り値。
func Judge(in JudgeInput) (JudgeResult, error) {
	if len(in.Days) > 0 && len(in.Days) != len(in.NetPnLJPY) {
		return JudgeResult{}, fmt.Errorf("judge: days length %d != net series length %d", len(in.Days), len(in.NetPnLJPY))
	}
	if in.FloorMultiple <= 0 {
		in.FloorMultiple = 1.0
	}
	if in.Confidence <= 0 || in.Confidence >= 1 {
		in.Confidence = 0.95
	}
	if in.Seed == 0 {
		in.Seed = 42
	}
	res := JudgeResult{N: len(in.NetPnLJPY), Mean: Mean(in.NetPnLJPY), NetPF: ProfitFactor(in.NetPnLJPY),
		OOSMean: math.NaN(), BenchExcess: math.NaN()}

	if res.N == 0 {
		res.Verdict = VerdictContinue
		res.Reasons = []string{"no_trades"}
		return res, nil
	}
	// 楽観側に出る CI を黙って返さない: 従来経路に落ちたら必ず reasons に注記する。
	var lo, hi float64
	var err error
	if len(in.Days) > 0 {
		res.DayBlocks = distinctDays(in.Days)
		lo, hi, err = BootstrapMeanCIDayBlock(in.NetPnLJPY, in.Days, in.Resamples, in.Confidence, in.Seed)
	} else {
		lo, hi, err = BootstrapMeanCI(in.NetPnLJPY, in.Resamples, in.Confidence, in.Seed)
		res.Reasons = append(res.Reasons, "trade_level_bootstrap_anti_conservative")
	}
	if err == nil {
		res.CILo, res.CIHi = lo, hi
	}

	minN := 100
	if in.Track == "A" {
		minN = 300
	}

	if res.N < minN {
		res.Verdict = VerdictContinue
		res.Reasons = append(res.Reasons, fmt.Sprintf("insufficient_n_track_%s (%d < %d)", track(in.Track), res.N, minN))
		return res, nil
	}
	// **トレード数を満たしても暦日が少なければ判定しない** — CI が潰れて統計ゲートが
	// 無効化され、黙って通す方向に壊れるため。
	if res.DayBlocks > 0 && res.DayBlocks < minDayBlocks {
		res.Verdict = VerdictContinue
		res.Reasons = append(res.Reasons, fmt.Sprintf("insufficient_day_blocks (%d < %d) — 実効標本は日数であってトレード数ではない", res.DayBlocks, minDayBlocks))
		return res, nil
	}
	if res.CIHi < 0 {
		res.Verdict = VerdictReject
		res.Reasons = append(res.Reasons, "ci_upper_negative")
		return res, nil
	}
	// 嘘発見器: an edge concentrated in too few symbols is not breadth.
	if in.UniverseN > 0 && in.UniverseN < 3 {
		res.Verdict = VerdictContinue
		res.Reasons = append(res.Reasons, fmt.Sprintf("universe_too_narrow (%d)", in.UniverseN))
		return res, nil
	}
	if res.Mean < in.CostFloorJPY*in.FloorMultiple {
		res.Verdict = VerdictContinue
		res.Reasons = append(res.Reasons, fmt.Sprintf("net_expectancy_below_cost_floor_multiple (%.1f < %.1f)", res.Mean, in.CostFloorJPY*in.FloorMultiple))
		return res, nil
	}
	oosRan := false
	if len(in.OOSNetPnLJPY) > 0 {
		oosRan = true
		res.OOSMean = Mean(in.OOSNetPnLJPY)
		if res.OOSMean <= 0 || sign(res.OOSMean) != sign(res.Mean) {
			res.Verdict = VerdictContinue
			res.Reasons = append(res.Reasons, "oos_sign_mismatch")
			return res, nil
		}
	} else if in.Track == "A" {
		res.Verdict = VerdictContinue
		res.Reasons = append(res.Reasons, "oos_absent_track_A_cannot_promote")
		return res, nil
	} else {
		res.Reasons = append(res.Reasons, "oos_absent_not_validated")
	}
	if len(in.BenchNetPnLJPY) > 0 {
		res.BenchExcess = res.Mean - Mean(in.BenchNetPnLJPY)
		if res.BenchExcess <= 0 {
			res.Verdict = VerdictContinue
			res.Reasons = append(res.Reasons, "no_excess_over_bench")
			return res, nil
		}
	}
	if res.CILo > 0 && res.NetPF >= 1.1 && (in.Track != "A" || oosRan) {
		res.Verdict = VerdictCandidate
		res.Reasons = append(res.Reasons, "passed_all_gates")
		return res, nil
	}
	res.Verdict = VerdictContinue
	res.Reasons = append(res.Reasons, "ci_lo_not_positive_or_pf_below_1.1")
	return res, nil
}

func distinctDays(days []string) int {
	seen := make(map[string]struct{}, len(days))
	for _, d := range days {
		seen[d] = struct{}{}
	}
	return len(seen)
}

// ProfitFactor は 勝ちの和 / 負けの和。空(勝ちも負けも 0)は 0、負けゼロは +Inf。
func ProfitFactor(vals []float64) float64 {
	var win, loss float64
	for _, v := range vals {
		if v > 0 {
			win += v
		} else {
			loss += -v
		}
	}
	switch {
	case loss == 0 && win == 0:
		return 0
	case loss == 0:
		return math.Inf(1)
	default:
		return win / loss
	}
}

func sign(v float64) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	default:
		return 0
	}
}

func track(t string) string {
	if t == "" {
		return "B"
	}
	return t
}
