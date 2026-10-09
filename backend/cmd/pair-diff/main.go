// cmd/pair-diff はペア比較の**本命の統計量**を出す。
//
// 4 つのトレンド系戦略について、入口が完全に同一で出口だけが違う
// 兄弟アーム(`X` = 固定 TP / `X_trail` = トレール)を並走させている。問いは
// 「固定 TP と トレール のどちらが良いか」で、答えは**ペア差**
// (同一トリガーの trail net − capped net)を day-block bootstrap に掛けて出す。
//
// 🛑 **アーム単独の net を 2 群で比べてはいけない**。両アームは同じ銘柄・同じ向き・
// 同じ時刻を持ち損益が強く相関するので、独立を仮定した検定になる。対応のある比較なら
// 相関が高いほど差の分散が小さくなり、**検出力はむしろ上がる**。
//
// READ-ONLY: SELECT しかしない。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"stockbot/backend/internal/adapter/repository/pg"
	"stockbot/backend/internal/backtest/judge"
	"stockbot/backend/internal/backtest/pairdiff"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/position"
)

// 兄弟ペア(基のアーム名で書く。兄弟は `_trail` を付けて導く)。
//
// トレンド系 4 ペアに加えて **`bnf_reversion`** も入れる —— メニューに実在する
// 5 組目の兄弟で、(銘柄, 戦略) キー(1 銘柄に戦略ごとの config)の効果を
// **最初に確かめられる**ペアでもある(銘柄キーのままだと「ペア 0 件」になる組)。
// 台帳側(ListPairTrades)は全戦略を返すので、ここに 1 行足せば遡って計算できる。
var basePairs = []string{
	"abs_momentum_v2",
	"atr_breakout_v2",
	"bnf_reversion",
	// bnf ファミリーの新入口 2 つ。
	"bnf_day2_reversion",
	"bnf_stabilized_reversion",
	// 日中版の兄弟(TP → 同距離の ratchet)。
	"bnf_intraday_reversion",
	"donchian_breakout_v2",
	"high_52w_momentum",
}

// minDayBlocks は judge と同じ 20。**実効標本は日数であってトレード数ではない。**
// 同じ日の複数トレードは相関するので、日をブロックにして抽出する。
const minDayBlocks = 20

// minPairs は判定を出すのに要るペア数。**新しい数字ではない** —
// 締めの参考閾値を「`cmd/edge-judge` が実際に持っているもの
// (Track B で **N≥100** かつ **day_blocks≥20**)」と事前登録しており、
// `judge.minN` と同じ値。day_blocks 側しか見ないと、
// **20 営業日ぶんの日付がありさえすれば 25 組でも `trail_better` を印字する**。
// 本命の統計量を出す道具のほうが緩いと、採点者は緩い方を先に読む。
const minPairs = 100

type armReport struct {
	Base        string         `json:"base"`
	PairsN      int            `json:"pairs_n"`
	BrokenN     int            `json:"broken_n"`
	BrokenByArm map[string]int `json:"broken_by_arm"`
	DayBlocks   int            `json:"day_blocks"`
	MeanDiffJPY float64        `json:"mean_diff_jpy"`
	// 🚨 突合の残差。事前登録は「同一建値」なので、0 でないぶんは
	// 事前登録からの逸脱そのもの。**判定と同じ画面に必ず出す。**
	MaxEntryGapPct  float64  `json:"max_entry_gap_pct"`
	MeanEntryGapPct float64  `json:"mean_entry_gap_pct"`
	MeanDiffPer1M   float64  `json:"mean_diff_per_1m_jpy"`
	CILoPer1M       *float64 `json:"ci_lo_per_1m_jpy,omitempty"`
	CIHiPer1M       *float64 `json:"ci_hi_per_1m_jpy,omitempty"`
	Verdict         string   `json:"verdict"`
	Reasons         []string `json:"reasons,omitempty"`
	// PendingN は**兄弟脚がまだ建玉中**のトリガー。BrokenN(建たなかった)と足さない。
	// 🚨 これを分けないと、ペア差が打ち切りで系統的に trail 不利へ偏り、
	// BrokenByArm がコスト床検出器として読めなくなる。
	PendingN        int                  `json:"pending_n"`
	SampleUnmatched []pairdiff.Unmatched `json:"sample_unmatched,omitempty"`
}

func main() {
	sinceStr := flag.String("since", "", "この JST 日付以降に決済されたトレードだけ(YYYY-MM-DD。空 = 全期間)")
	db := flag.String("db", "research", "どちらの台帳を読むか: research(STOCKBOT_DATABASE_URL)| harvest(STOCKBOT_HARVEST_DATABASE_URL)| live(STOCKBOT_LIVE_DATABASE_URL)")
	asJSON := flag.Bool("json", false, "JSON で stdout へ")
	resamples := flag.Int("resamples", 10000, "bootstrap のリサンプル回数")
	seed := flag.Int64("seed", 42, "bootstrap の乱数 seed(既定固定 = 同じ台帳なら同じ CI)")
	flag.Parse()

	url, err := config.LedgerDSN(*db)
	if err != nil {
		fatal(err)
	}
	since := time.Time{}
	if *sinceStr != "" {
		d, err := time.ParseInLocation("2006-01-02", *sinceStr, clock.JST)
		if err != nil {
			fatal(fmt.Errorf("-since は YYYY-MM-DD: %w", err))
		}
		since = d
	}

	ctx := context.Background()
	pool, err := pg.Open(ctx, url)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()

	repo := pg.NewPairRepo(pool)
	rows, err := repo.ListPairTrades(ctx, since)
	if err != nil {
		fatal(err)
	}
	// 🚨 **まだ決済されていない脚**も読む。これが無いと「兄弟脚が建玉中」を
	// 「建たなかった」と誤ラベルする。
	// 🛑 **建玉中の脚は since で切らない**。決済側は
	// `closed_at >= since`、建玉側は `opened_at >= since` なので、**since より前に
	// 建って since 以降に決済したペア**の片脚がまだ open のとき、その open 脚が
	// 母集団から落ちて「建たなかった」に化ける。open 建玉は多くても数百件なので
	// 全件読んでよい。
	openRows, err := repo.ListOpenPairLegs(ctx, time.Time{})
	if err != nil {
		fatal(err)
	}
	openLegs := make([]pairdiff.OpenLeg, 0, len(openRows))
	for _, o := range openRows {
		openLegs = append(openLegs, pairdiff.OpenLeg{
			Symbol: o.Symbol, Strategy: o.Strategy, EntryPrice: o.EntryPrice,
			Day: o.OpenedAt.In(clock.JST).Format("2006-01-02"),
		})
	}

	// 🛑 ブロックキーは**入口の日**。出口の日で束ねると、トレールが伸びたペアほど
	// 別ブロックに散って相関構造が壊れる。
	trades := make([]pairdiff.Trade, 0, len(rows))
	for _, r := range rows {
		trades = append(trades, pairdiff.Trade{
			Symbol: r.Symbol, Strategy: r.Strategy, EntryPrice: r.EntryPrice,
			NetJPY: r.NetJPY, Day: r.OpenedAt.In(clock.JST).Format("2006-01-02"),
		})
	}

	// 🚨 検出器の**読む側**。コスト床は trail 側だけ 3 倍厳しいので、
	// `tp_below_cost_floor` の件数は trail に偏るはず。その比を出さないと、
	// 「ペアが揃ったものだけを見た結論」が高コスト銘柄を落とした後の話だと分からない。
	rejections, err := repo.CountRejectionsByArm(ctx, since)
	if err != nil {
		fatal(err)
	}

	reports := make([]armReport, 0, len(basePairs))
	for _, base := range basePairs {
		reports = append(reports, buildReport(base, rows, trades, openLegs, *resamples, *seed))
	}

	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"arms": reports, "rejections_by_arm": rejections,
		}, "", "  ")
		os.Stdout.Write(append(b, '\n'))
		return
	}
	printHuman(reports, *db, *sinceStr)
	printRejections(rejections)
}

// buildReport は 1 ペアぶん。¥1M 正規化はペア差そのものに掛ける
// (建玉金額が違う銘柄の差を素で平均すると、値がさ株 1 件が全体を決める)。
func buildReport(base string, rows []pg.PairTradeRow, trades []pairdiff.Trade,
	open []pairdiff.OpenLeg, resamples int, seed int64) armReport {
	res := pairdiff.MatchWithOpen(base, trades, open)
	rep := armReport{
		Base: base, PairsN: len(res.Pairs), BrokenN: len(res.Unmatched),
		PendingN:    len(res.Pending),
		BrokenByArm: res.BrokenByArm(), DayBlocks: res.DayBlocks,
		MeanDiffJPY:     res.MeanDiffJPY,
		MaxEntryGapPct:  res.MaxEntryGapPct,
		MeanEntryGapPct: res.MeanEntryGapPct,
	}
	// 株数は行側にしか無いので (銘柄, 建値, 入口日) で引き直す。
	qty := map[string]int{}
	for _, r := range rows {
		k := fmt.Sprintf("%s|%.2f|%s", r.Symbol, r.EntryPrice, r.OpenedAt.In(clock.JST).Format("2006-01-02"))
		qty[k] = r.Quantity
	}
	diffs1m := make([]float64, 0, len(res.Pairs))
	days := make([]string, 0, len(res.Pairs))
	for _, p := range res.Pairs {
		k := fmt.Sprintf("%s|%.2f|%s", p.Symbol, p.EntryPrice, p.Day)
		q, ok := qty[k]
		if !ok || q == 0 || p.EntryPrice <= 0 {
			continue // 建玉金額が出せない行は落とす(0 で埋めない)
		}
		v := position.Per1MNotional(p.Diff, p.EntryPrice, q)
		diffs1m = append(diffs1m, v)
		days = append(days, p.Day)
	}
	if len(diffs1m) > 0 {
		rep.MeanDiffPer1M = judge.Mean(diffs1m)
	}

	if len(diffs1m) == 0 {
		rep.Verdict = "no_pairs"
		rep.Reasons = append(rep.Reasons, "ペアが 1 組も成立していない — 兄弟アームが同時に建っていない可能性(キー / コスト床を疑う)")
	} else if rep.DayBlocks < minDayBlocks {
		rep.Verdict = "insufficient_day_blocks"
		rep.Reasons = append(rep.Reasons, fmt.Sprintf(
			"day_blocks %d < %d — 実効標本は日数であってトレード数ではない。判定を読まない",
			rep.DayBlocks, minDayBlocks))
	} else if rep.PairsN < minPairs {
		// 🛑 day_blocks が足りても N が足りなければ判定しない(事前登録は **両方**)。
		rep.Verdict = "insufficient_n"
		rep.Reasons = append(rep.Reasons, fmt.Sprintf(
			"ペア %d < %d — day_blocks は足りているが標本が足りない。edge-judge と同じ閾値(事前登録)",
			rep.PairsN, minPairs))
	}
	if len(diffs1m) >= 2 {
		lo, hi, err := judge.BootstrapMeanCIDayBlock(diffs1m, days, resamples, 0.95, seed)
		if err == nil {
			rep.CILoPer1M, rep.CIHiPer1M = &lo, &hi
			if rep.Verdict == "" {
				switch {
				case lo > 0:
					rep.Verdict = "trail_better"
				case hi < 0:
					rep.Verdict = "capped_better"
				default:
					rep.Verdict = "inconclusive"
					rep.Reasons = append(rep.Reasons, "CI が 0 を跨ぐ — どちらが良いかは言えない")
				}
			}
		}
	}
	if rep.Verdict == "" {
		rep.Verdict = "insufficient_n"
	}

	// 🚨 壊れたペアは**必ず出す**。全部は多いので先頭数件だけ例として出す。
	sort.Slice(res.Unmatched, func(i, j int) bool { return res.Unmatched[i].Day < res.Unmatched[j].Day })
	if n := len(res.Unmatched); n > 0 {
		if n > 5 {
			n = 5
		}
		rep.SampleUnmatched = res.Unmatched[:n]
	}
	return rep
}

func printHuman(reports []armReport, db, since string) {
	fmt.Printf("── ペア差(固定TP vs トレール)── 台帳=%s", db)
	if since != "" {
		fmt.Printf(" / since=%s", since)
	}
	fmt.Println()
	fmt.Println("  統計量は **同一トリガーの (trail net − capped net)**。アーム単独の net はその内訳。")
	fmt.Println()
	for _, r := range reports {
		fmt.Printf("● %s\n", r.Base)
		fmt.Printf("    ペア成立 %d 組 / **建たなかった** %d 件 / 兄弟脚がまだ建玉中 %d 件 / day_blocks %d\n",
			r.PairsN, r.BrokenN, r.PendingN, r.DayBlocks)
		if len(r.BrokenByArm) > 0 {
			for arm, n := range r.BrokenByArm {
				fmt.Printf("      └ %s が建たなかった: %d 件\n", arm, n)
			}
		}
		if r.PairsN > 0 {
			fmt.Printf("    平均ペア差 %+.0f 円 / ¥1M 正規化 %+.0f 円\n", r.MeanDiffJPY, r.MeanDiffPer1M)
		}
		if r.CILoPer1M != nil {
			fmt.Printf("    95%% CI(¥1M) [%+.0f, %+.0f]\n", *r.CILoPer1M, *r.CIHiPer1M)
		}
		// 🚨 事前登録は「同一銘柄・**同一建値**」。実装は建値 2% 許容の最近傍なので、
		// **どれだけずれた組を数えたか**を判定と同じ画面に必ず出す(無開示だと事後の裁量)。
		if r.PairsN > 0 {
			fmt.Printf("    突合の残差: 建値ずれ 平均 %.2f%% / 最大 %.2f%%(事前登録は「同一建値」= 0%%・許容 2%%)\n",
				r.MeanEntryGapPct*100, r.MaxEntryGapPct*100)
			if r.MaxEntryGapPct > 0.015 {
				fmt.Printf("      ⚠ 最大が許容(2%%)に近い — 「同じトリガー」ではなく別の水準で建った組を数えている疑い\n")
			}
		}
		fmt.Printf("    判定: %s\n", r.Verdict)
		for _, why := range r.Reasons {
			fmt.Printf("      ⚠ %s\n", why)
		}
		fmt.Println()
	}
	fmt.Println("  🛑 ペアは独立ではない(同じトリガー)。ポートフォリオ全体の net としては二重計上になる —")
	fmt.Println("     「戦略が13個」ではなく 入口7 / うち5に出口2通り = 12アーム(ペア差は5組)と読む。")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "pair-diff:", err)
	os.Exit(1)
}

// printRejections は検出器の出力。**アーム別に数えて黙って落とさない。**
func printRejections(rs []pg.ArmRejection) {
	if len(rs) == 0 {
		return
	}
	fmt.Printf("\n── signal_rejections(アーム別 × 理由別)── コスト床の検出器\n")
	fmt.Println("   🛑 コスト床は trail 側だけ 3 倍厳しい(対象 1.0×ATR / v2 は 3.0×ATR)ので、")
	fmt.Println("      `tp_below_cost_floor` は trail に偏るのが**正しい**。偏りの大きさを採点時に開示する。")
	cur := ""
	for _, r := range rs {
		if r.Strategy != cur {
			cur = r.Strategy
			fmt.Printf("  %s\n", cur)
		}
		fmt.Printf("      %-28s %d\n", r.Reason, r.N)
	}
}
