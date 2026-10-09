// Command counterfactual は「時間で打ち切った建玉が、そのまま持っていたら TP と SL の
// どちらに当たっていたか」を決済後の日足から事後に計算する。
//
// これが無いと MaxHold キャップや期間の締めの手仕舞いの是非は**原理的に判定できない**
// — 閉じた後の道は観測できないため。締めの手仕舞いになった建玉は、その全部が
// 「自然な出口に到達していない標本」。
//
// READ-ONLY: SELECT と CSV 読みだけ。発注 API も書込 SQL も型として存在しない。
//
// 🛑 **出るのは推定であって実現損益ではない。** 台帳の net と混ぜない。決着(バリア到達)と
// 含み(未決着の mark-to-market)も足さない — 含みは全銘柄が同じ日で切られた 1 回のドローで、
// 戦略間で完全に相関する。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"stockbot/backend/internal/adapter/candlecsv"
	"stockbot/backend/internal/adapter/repository/pg"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
)

func main() {
	sinceStr := flag.String("since", "", "この JST 日付以降に決済した建玉(YYYY-MM-DD)。**サイクル境界で切ること**")
	untilStr := flag.String("until", "", "この JST 日付より前に決済した建玉(YYYY-MM-DD。空 = 今日まで)")
	// harvest_expiry(migration 0018)は**後から課した保有期限による打ち切り** = 検閲標本。
	// 「手仕舞わなければどうだったか」を測るこのツールの本命なので既定に入れる。
	reasonsCSV := flag.String("reasons", "max_hold,manual,forced_flat,harvest_expiry", "対象の close_reason(カンマ区切り。空 = 全件)")
	dataDir := flag.String("data", "data", "<sym>_daily.csv を置いたディレクトリ")
	db := flag.String("db", "research", "どちらの台帳を読むか: research(STOCKBOT_DATABASE_URL)| harvest(STOCKBOT_HARVEST_DATABASE_URL)| live(STOCKBOT_LIVE_DATABASE_URL)")
	floor := flag.Bool("floor", false, "トレール建玉を**床あり**(max(arm, peak−giveback))で歩く。既定は固定 TP/SL")
	asJSON := flag.Bool("json", false, "明細を JSON で stdout へ")
	flag.Parse()

	url, err := config.LedgerDSN(*db)
	if err != nil {
		fatal(err)
	}
	from, to := time.Time{}, clock.System()().AddDate(0, 0, 1)
	if *sinceStr != "" {
		if from, err = time.ParseInLocation("2006-01-02", *sinceStr, clock.JST); err != nil {
			fatal(fmt.Errorf("-since は YYYY-MM-DD: %w", err))
		}
	}
	if *untilStr != "" {
		if to, err = time.ParseInLocation("2006-01-02", *untilStr, clock.JST); err != nil {
			fatal(fmt.Errorf("-until は YYYY-MM-DD: %w", err))
		}
	}
	var reasons []string
	for _, r := range strings.Split(*reasonsCSV, ",") {
		if r = strings.TrimSpace(r); r != "" {
			reasons = append(reasons, r)
		}
	}

	ctx := context.Background()
	pool, err := pg.Open(ctx, url)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()
	closed, err := pg.NewJournalRepo(pool).ClosedPositions(ctx, from, to, reasons)
	if err != nil {
		fatal(err)
	}

	floorMode = *floor
	cache := map[string][]market.Candle{}
	rep := analyse(closed, func(symbol string) ([]market.Candle, error) {
		if cs, ok := cache[symbol]; ok {
			return cs, nil
		}
		cs, err := candlecsv.LoadDaily(*dataDir, symbol)
		if err != nil {
			return nil, err
		}
		cache[symbol] = cs
		return cs, nil
	})

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fatal(err)
		}
		return
	}
	mode := "固定 TP/SL"
	if *floor {
		mode = "**床あり**(トレール建玉は max(arm, peak−giveback) で歩く)"
	}
	fmt.Printf("── counterfactual(打ち切った建玉を、そのまま持っていたら)/ 出口の当て方: %s ──\n", mode)
	fmt.Printf("対象 %d本(close_reason: %s)/ 決済日 %s〜%s / 含みは %s の終値で評価\n",
		len(closed), *reasonsCSV, orDash(rep.FirstClose), orDash(rep.LastClose), orDash(rep.AsOfBar))
	fmt.Printf("%-24s %4s %5s %5s %6s %5s %5s %6s %5s %13s %13s %13s\n",
		"戦略", "N", "→TP", "→SL", "→期限", "両方", "未決", "材料無", "除外", "実現 net", "決着 gross", "含み gross")
	for _, s := range rep.ByStrategy {
		fmt.Printf("%-24s %4d %5d %5d %6d %5d %5d %6d %5d %13.0f %13.0f %13.0f\n",
			s.Strategy, s.N, s.TakeProfit, s.StopLoss, s.MaxHoldHit, s.Ambiguous, s.Unresolved, s.NoData, s.Skipped,
			s.RealisedJPY, s.SettledJPY, s.UnrealisedJPY)
	}
	for _, w := range rep.Warnings {
		fmt.Printf("\n⚠ %s\n", w)
	}
	fmt.Println("\n🛑 「→期限」= 凍結された MaxHold に達していた本数。手仕舞いしなくても期限で閉じていた標本で、")
	fmt.Println("   **「持っていれば伸びた」とは読めない**(決済理由が max_hold の行だけはキャップ無しで歩いている)。")
	fmt.Println("\n🛑 反実仮想は推定であって実現損益ではない(手数料・金利を含まない)。台帳の net と混ぜない。")
	fmt.Println("🛑 「決着 gross」と「含み gross」を足さない — 含みは全銘柄が同じ日で切られた 1 回のドローで、")
	fmt.Println("   戦略間で完全に相関する。「両方」「材料無」「除外」は損益に寄与しない(N には数える)。")
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "counterfactual:", err)
	os.Exit(1)
}
