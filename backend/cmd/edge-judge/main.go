// Command edge-judge applies the statistical pass/reject rules to a net-PnL
// series. Input is a JSON file of per-trade net JPY (and optional OOS
// / benchmark series). Output is a human block + a final JSON verdict line.
// AI范围 is "提示まで": a promote_candidate verdict is a recommendation; the live
// decision is the human's.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"stockbot/backend/internal/backtest/judge"
	"stockbot/backend/internal/usecase/query"
)

type input struct {
	Track string `json:"track"`
	// Counting は入力がどちらの数え方かの名乗り(forward-report / ダッシュボードが出す)。
	// 空 = 名乗りの無い古い入力。判定に流せるのはエッジ標本だけ(checkCounting)。
	Counting string `json:"counting"`
	// NetPer1MJPY は ¥1M notional 正規化系列(EDGE_METHODOLOGY §2-3「コード強制」)。
	// **在れば必ずこちらを判定に使う** — 生の net_pnl_jpy は建玉金額が銘柄で 20 倍
	// 以上ばらつくため、「値がさ株を掴んだ戦略」が強く見える。net_pnl_jpy は
	// 正規化系列を持たない古い入力との後方互換のためだけに残す。
	NetPer1MJPY []float64 `json:"net_per_1m_jpy"`
	NetPnLJPY   []float64 `json:"net_pnl_jpy"`
	// Days は net 系列と同順同長の JST 日付。forward-report -json が出す。
	// 在れば CI は day-block bootstrap(日クラスタを保つ)= 本来の保守的な CI。
	// 無ければトレード単位に落ちるが、その事実は verdict の reasons に出る。
	Days           []string  `json:"days"`
	OOSNetPnLJPY   []float64 `json:"oos_net_pnl_jpy"`
	BenchNetPnLJPY []float64 `json:"bench_net_pnl_jpy"`
	CostFloorJPY   float64   `json:"cost_floor_jpy"`
	FloorMultiple  float64   `json:"floor_multiple"`
	UniverseN      int       `json:"universe_n"`
}

func main() {
	path := flag.String("in", "", "JSON file with net_pnl_jpy[] (and optional oos/bench/cost_floor)")
	flag.Parse()
	if *path == "" {
		fmt.Fprintln(os.Stderr, "usage: edge-judge -in net.json")
		os.Exit(2)
	}
	b, err := os.ReadFile(*path)
	if err != nil {
		fatal(err)
	}
	var in input
	if err := json.Unmarshal(b, &in); err != nil {
		fatal(err)
	}
	if err := checkCounting(in.Counting); err != nil {
		fatal(err)
	}
	net, unit := in.NetPnLJPY, "生の円(正規化系列なし)"
	if len(in.NetPer1MJPY) > 0 {
		net, unit = in.NetPer1MJPY, "¥1M notional 正規化"
	}
	res, err := judge.Judge(judge.JudgeInput{
		Track: in.Track, NetPnLJPY: net, Days: in.Days, OOSNetPnLJPY: in.OOSNetPnLJPY,
		BenchNetPnLJPY: in.BenchNetPnLJPY, CostFloorJPY: in.CostFloorJPY, FloorMultiple: in.FloorMultiple,
		UniverseN: in.UniverseN,
	})
	if err != nil {
		fatal(err)
	}

	ciKind := "day-block bootstrap(日クラスタ保持)"
	if len(in.Days) == 0 {
		ciKind = "トレード単位 bootstrap ⚠ days 欠落 — CI は楽観側(狭すぎ)"
	}
	fmt.Fprintf(os.Stderr, "── edge-judge ───────────────\n")
	fmt.Fprintf(os.Stderr, "単位: %s\n", unit)
	fmt.Fprintf(os.Stderr, "CI: %s\n", ciKind)
	// 実効標本は**日数**であってトレード数ではない(222銘柄 forward はトレードが
	// 日単位でクラスタする)。両方を並べて出さないと N=380 を独立標本と読み違える。
	fmt.Fprintf(os.Stderr, "N=%d トレード / 実効標本 %d 営業日  net_expectancy=%.2f JPY  CI=[%.2f, %.2f]  netPF=%.3f\n",
		res.N, res.DayBlocks, res.Mean, res.CILo, res.CIHi, res.NetPF)
	fmt.Fprintf(os.Stderr, "verdict: %s\n", res.Verdict)
	for _, r := range res.Reasons {
		fmt.Fprintf(os.Stderr, "  - %s\n", r)
	}
	if res.Verdict == judge.VerdictCandidate {
		fmt.Fprintf(os.Stderr, "⚠ promote_candidate は提示のみ。live 投入の可否・ロットは人間が判断する。\n")
	}
	fmt.Fprintf(os.Stderr, "─────────────────────────────\n")

	// err を捨てない: 捨てると NaN(OOS/bench 不在)の
	// エンコード失敗が黙殺され、この verdict 行が一度も出ない。
	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		fatal(err)
	}
}

// checkCounting は「この系列を判定に流してよいか」。ダッシュボードの
// `/api/performance` は**口座ベース**(entry_compensated /
// external_close も戦績に計上)で、**同じ形の JSON** を返す。取り違えて -in に渡すと
// 「戦略の出口ではない往復」が入った系列で verdict が出て、事前コミット
// した標本の定義が静かに崩れる。名乗りが違う入力は**読まずに落とす**。
//
// 名乗りの無い入力(手書きの net.json / 過去の forward-report 出力)は通す —
// 名乗りが無いことを理由に判定できなくしない。
func checkCounting(counting string) error {
	switch counting {
	case "", query.CountingEdgeSample:
		return nil
	default:
		return fmt.Errorf("counting=%q の入力は判定に使えません(エッジ標本は %q)。"+
			"ダッシュボードの /api/performance は口座ベースで entry_compensated / external_close を"+
			"戦績に計上しているので、判定には `forward-report -json` の出力を渡してください。",
			counting, query.CountingEdgeSample)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "edge-judge:", err)
	os.Exit(1)
}
