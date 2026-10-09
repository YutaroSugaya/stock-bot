// Command universe-screen re-screens the local daily CSVs down to the tradable
// research universe and prints the pass/fail verdict for every symbol.
//
// READ-ONLY and OFFLINE apart from ONE file (-out). No API, no orders, no config
// writes — hard_limits.allowed_symbols stays a human commit and this tool can only
// narrow INSIDE it (-allow-file)。
//
// 選定規則そのものは台帳の事前登録。サイクル中に閾値や -top-n を変えるのは事後の
// 勝ち探しなので、変えるなら新規検定として登録する。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"stockbot/backend/internal/adapter/candlecsv"
	"stockbot/backend/internal/backtest/universe"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
)

func main() {
	dir := flag.String("data", "data", "<sym>_daily.csv を置いたディレクトリ")
	minTurnover := flag.Float64("min-turnover-jpy", 5_000_000_000,
		"売買代金中央値の下限(円/日)。研究の質の下限で、資金が増えても緩めない")
	maxLot := flag.Float64("max-lot-jpy", 1_000_000,
		"最小単元(100株)の建玉金額の上限(円)。資金適合の条件で、資金が増えれば緩む")
	lookback := flag.Int("lookback", 60, "売買代金中央値をとる営業日数")
	topN := flag.Int("top-n", 0, "売買代金中央値の上位N銘柄だけ採用(0 = 無制限)")
	allowFile := flag.String("allow-file", "",
		"hard_limits.yaml のパス。指定するとその allowed_symbols の内側だけを候補にする(推奨)")
	out := flag.String("out", "",
		"選定結果を1行1銘柄で書き出すパス(原子的に置換)。空 = 書かない")
	minCount := flag.Int("min-count", 0,
		"-out の安全弁: 選定結果がこの件数未満なら既存ファイルを書き換えない(0 = 空だけ拒否)")
	maxStale := flag.Int("max-stale-days", 7,
		"最終バーがデータセット最新からこの日数以上遅れている銘柄を stale で落とす(0 = 無効)。データセット全体が今日からこれ以上古ければ -out は書かない")
	archiveDir := flag.String("archive-dir", "",
		"選定結果を <dir>/YYYY-MM-DD.txt にも残す(事後再現用)。空 = 残さない")
	asYAML := flag.Bool("yaml", false, "通過銘柄を configs に貼れる YAML 配列で出す")
	verbose := flag.Bool("verbose", false, "落ちた銘柄を全件表示する(既定は要約 + 境界付近のみ)")
	flag.Parse()

	// Go の flag は `-top-n -5` をパースエラーにしない。`topN <= 0` は「無制限」なので
	// 負値は全銘柄(1,500超)を通し、しかも上位N の見出し行が消えてログ上は正常に見える。
	for _, f := range []struct {
		name string
		val  float64
	}{{"top-n", float64(*topN)}, {"min-count", float64(*minCount)}, {"lookback", float64(*lookback)},
		{"max-stale-days", float64(*maxStale)}, {"min-turnover-jpy", *minTurnover}, {"max-lot-jpy", *maxLot}} {
		if f.val < 0 {
			fmt.Fprintf(os.Stderr, "universe-screen: -%s に負値 (%g) は指定できません\n", f.name, f.val)
			os.Exit(2)
		}
	}
	if err := validateOutFlags(*out, *topN, *minCount); err != nil {
		fmt.Fprintln(os.Stderr, "universe-screen:", err)
		os.Exit(2)
	}
	// -yaml は早期 return するので、併記すると「書いたつもりで書かれていない」になる。
	if *out != "" && *asYAML {
		fmt.Fprintln(os.Stderr, "universe-screen: -yaml と -out は同時に使えません(-yaml は人間が貼る用・-out は運用ファイル)")
		os.Exit(2)
	}

	syms, err := candlecsv.DailySymbols(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "universe-screen: %s の走査に失敗: %v\n", *dir, err)
		os.Exit(2)
	}
	if len(syms) == 0 {
		fmt.Fprintf(os.Stderr, "universe-screen: %s に *_daily.csv がありません\n", *dir)
		os.Exit(2)
	}
	u := make(map[string][]market.Candle, len(syms))
	for _, sym := range syms {
		if strings.Contains(sym, "bench") {
			continue // ベンチマーク系列は銘柄ではない
		}
		cs, err := candlecsv.LoadDaily(*dir, sym)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠ %s: 読めません (%v)\n", sym, err)
			continue
		}
		u[sym] = cs
	}
	candidates := len(u)

	// 「ホワイトリストを読めなかったので全銘柄を候補にした」が最悪の縮退なので fail-close。
	var denied []string
	if *allowFile != "" {
		hl, err := config.LoadHardLimits(*allowFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "universe-screen: allowed_symbols を読めません: %v\n", err)
			os.Exit(2)
		}
		u, denied = filterAllowed(u, hl.AllowedSymbols)
		if len(u) == 0 {
			fmt.Fprintf(os.Stderr, "universe-screen: 候補 %d 銘柄が全て allowed_symbols の外(%s)— 選定できません\n",
				candidates, *allowFile)
			os.Exit(2)
		}
	}

	rs := universe.Screen(u, universe.Criteria{
		MedianTurnoverJPY: *minTurnover,
		MaxLotNotionalJPY: *maxLot,
		Lookback:          *lookback,
		TopN:              *topN,
		MaxStaleDays:      *maxStale,
	})
	passed := universe.Passed(rs)

	if *asYAML {
		fmt.Println("symbols: [")
		for i, s := range passed {
			if i%10 == 0 {
				fmt.Print("  ")
			}
			fmt.Printf("%q, ", s)
			if i%10 == 9 {
				fmt.Println()
			}
		}
		fmt.Println("\n]")
		return
	}

	byReason := map[string][]universe.Result{}
	for _, r := range rs {
		if !r.Passed {
			byReason[r.Reason] = append(byReason[r.Reason], r)
		}
	}
	fmt.Printf("── universe-screen(%s・直近%d営業日)──\n", *dir, *lookback)
	fmt.Printf("条件: 売買代金中央値 ≥ %.0f億円/日  ×  単元(100株)≤ %.0f万円", *minTurnover/1e8, *maxLot/1e4)
	if *topN > 0 {
		fmt.Printf("  ×  流動性上位 %d", *topN)
	}
	fmt.Println()
	if *allowFile != "" {
		fmt.Printf("候補: %d 銘柄(うち %d 銘柄は allowed_symbols の外なので除外)\n", candidates, len(denied))
		if *verbose && len(denied) > 0 {
			fmt.Printf("  除外: %s\n", strings.Join(denied, " "))
		}
	}
	fmt.Printf("結果: %d / %d 銘柄が通過\n\n", len(passed), len(rs))

	// 上位は常に出す。境界だけ見ているとデータ異常で1位を取った銘柄が朝のログに
	// 一切現れない(実測: 285A の売買代金中央値が 3兆円/日)。
	printTopRanks(rs, 10)

	for _, reason := range []string{"illiquid", "lot_too_expensive", "below_top_n", "stale", "insufficient_history", "no_price"} {
		rows := byReason[reason]
		if len(rows) == 0 {
			continue
		}
		fmt.Printf("%s(%d銘柄)", label(reason), len(rows))
		show := rows
		if !*verbose {
			fmt.Print(" ※先頭のみ")
			if len(show) > 5 {
				show = show[:5]
			}
		}
		fmt.Println(":")
		for _, r := range show {
			printRow(r)
		}
		fmt.Println()
	}
	if *topN > 0 {
		printCutLine(rs, *topN)
	}
	fmt.Printf("通過 %d 銘柄。configs へ貼るには -yaml、毎朝の選定は -out。\n", len(passed))
	fmt.Printf("注意: 選定規則(閾値 / -top-n)は台帳の事前登録。サイクル中に動かさない。\n")

	// 書き込みは最後。ここより前で os.Exit すると、-min-count を割った朝(= まさに
	// 原因を知りたい朝)のログに理由別の内訳が1行も残らない。
	writeOut(*out, *archiveDir, passed, *minCount, *maxStale, u, clock.System())
}

// writeOut persists the selection, refusing when the dataset itself is stale.
//
// 鮮度は時計を持つこの層で見る(universe.Screen は純粋性のため銘柄間の相対的な遅れ
// しか見ない)。fetch-daily が no-op でも選定は古いランキングから mtime が今日の
// ファイルを書けてしまい、朝の更新が何日も連続で no-op になる事故と同じ形で
// 「毎朝更新されているように見えるユニバース」ができる。
func writeOut(out, archiveDir string, passed []string, minCount, maxStale int,
	u map[string][]market.Candle, clk clock.Clock) {
	if out == "" {
		return
	}
	now := clk()
	if maxStale > 0 {
		newest := newestBarDay(u)
		if newest.IsZero() {
			fmt.Fprintln(os.Stderr, "universe-screen: 日足に日付が無く鮮度を判定できません — 書き込みません")
			os.Exit(1)
		}
		if age := int(now.Sub(newest).Hours() / 24); age > maxStale {
			fmt.Fprintf(os.Stderr, "universe-screen: データセットの最新バーが %s(%d日前 > -max-stale-days %d)— %s は書き換えません(fetch-daily が止まっている疑い)\n",
				newest.Format("2006-01-02"), age, maxStale, out)
			os.Exit(1)
		}
	}
	if err := writeUniverseFile(out, passed, minCount); err != nil {
		fmt.Fprintf(os.Stderr, "universe-screen: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("選定 %d 銘柄 → %s\n", len(passed), out)
	if archiveDir != "" {
		day := now.Format("2006-01-02")
		if err := archiveUniverseFile(out, archiveDir, day); err != nil {
			// 控えが取れないだけで today.txt は正しく書けている。
			fmt.Fprintf(os.Stderr, "universe-screen: ⚠ 日付つきの控えを作れません(選定自体は成功): %v\n", err)
			return
		}
		fmt.Printf("控え: %s/%s.txt(事後再現用)\n", archiveDir, day)
	}
}

// newestBarDay is the most recent bar date in the dataset (UTC 日付で丸め)。
func newestBarDay(u map[string][]market.Candle) time.Time {
	var newest time.Time
	for _, cs := range u {
		if len(cs) == 0 {
			continue
		}
		if t := cs[len(cs)-1].OpenTime; t.After(newest) {
			newest = t
		}
	}
	return newest
}

// printTopRanks shows the most liquid symbols — where data corruption surfaces first.
func printTopRanks(rs []universe.Result, n int) {
	byRank := make(map[int]universe.Result, len(rs))
	for _, r := range rs {
		if r.Rank > 0 {
			byRank[r.Rank] = r
		}
	}
	fmt.Printf("── 上位%d(データ異常はここに出る)──\n", n)
	for rank := 1; rank <= n; rank++ {
		if r, ok := byRank[rank]; ok {
			printRow(r)
		}
	}
	fmt.Println()
}

func printRow(r universe.Result) {
	rank := "  -"
	if r.Rank > 0 {
		rank = fmt.Sprintf("%3d位", r.Rank)
	}
	fmt.Printf("  %-6s %s 株価 %9.1f円  単元 %7.0f万円  売買代金中央値 %6.1f億円/日\n",
		r.Symbol, rank, r.LastClose, r.LotNotionalJPY/1e4, r.MedianTurnoverJPY/1e8)
}

// printCutLine shows the symbols straddling the top-N boundary.
func printCutLine(rs []universe.Result, topN int) {
	byRank := make(map[int]universe.Result, len(rs))
	for _, r := range rs {
		if r.Rank > 0 {
			byRank[r.Rank] = r
		}
	}
	fmt.Printf("── 境界(上位%d の前後)──\n", topN)
	for rank := topN - 2; rank <= topN+3; rank++ {
		r, ok := byRank[rank]
		if !ok {
			continue
		}
		if rank == topN+1 {
			fmt.Println("  ---- ここから採用外 ----")
		}
		printRow(r)
	}
	fmt.Println()
}

func label(reason string) string {
	switch reason {
	case "illiquid":
		return "❌ 流動性不足(研究の質の下限 — 資金が増えても復活しない)"
	case "lot_too_expensive":
		return "💰 単元が高すぎ(資金適合 — 資金が増えれば復活しうる)"
	case "below_top_n":
		return "📉 上位N の外(閾値は満たすが順位で落選 — 明日入れ替わりうる)"
	case "stale":
		return "🕒 日足が古い(売買停止 / 上場廃止の疑い — 停止前の流動性で選ばれるのを防ぐ)"
	case "insufficient_history":
		return "⚠ 履歴不足(判定できない)"
	default:
		return "⚠ " + reason
	}
}
