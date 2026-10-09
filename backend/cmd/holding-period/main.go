// Command holding-period は保有期間の分布(戦略 × 決済理由)を出す。
//
// **この検定の第一の目的**は MaxHold が効くかではなく、現行の出口での保有期間分布を
// 測ること。固定率の旧出口は SL ≈ 建値 2%、ATR 出口は 2.0×ATR ≈ 9% で
// **バリアまでの距離が約 4.5 倍**違い、到達時間は距離の 2 乗で伸びるので転用できない。
//
// READ-ONLY: SELECT だけ。発注 API も書込 SQL も型として存在しない。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"stockbot/backend/internal/adapter/repository/pg"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
)

var jst = clock.JST

func main() {
	sinceStr := flag.String("since", "", "この JST 日付以降に決済した建玉(YYYY-MM-DD)。**サイクル境界で切ること**")
	untilStr := flag.String("until", "", "この JST 日付より前に決済した建玉(YYYY-MM-DD)")
	reasonsCSV := flag.String("reasons", "", "対象の close_reason(カンマ区切り。空 = 全件)")
	db := flag.String("db", "research", "どちらの台帳を読むか: research(STOCKBOT_DATABASE_URL)| harvest(STOCKBOT_HARVEST_DATABASE_URL)| live(STOCKBOT_LIVE_DATABASE_URL)")
	asJSON := flag.Bool("json", false, "JSON で stdout へ")
	flag.Parse()

	url, err := config.LedgerDSN(*db)
	if err != nil {
		fatal(err)
	}
	from, to := time.Time{}, clock.System()().AddDate(0, 0, 1)
	if *sinceStr != "" {
		if from, err = time.ParseInLocation("2006-01-02", *sinceStr, jst); err != nil {
			fatal(fmt.Errorf("-since は YYYY-MM-DD: %w", err))
		}
	}
	if *untilStr != "" {
		if to, err = time.ParseInLocation("2006-01-02", *untilStr, jst); err != nil {
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
	rep := distribution(closed)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fatal(err)
		}
		return
	}
	fmt.Println("── 保有期間の分布(戦略 × 決済理由)──")
	fmt.Printf("対象 %d 本 / 決済日 %s〜%s\n", rep.N, orDash(rep.FirstClose), orDash(rep.LastClose))
	fmt.Printf("%-24s %-20s %4s %8s %8s %8s %8s %8s %8s %13s\n",
		"戦略", "決済理由", "N", "中央(暦)", "p90(暦)", "最大(暦)", "中央(営)", "p90(営)", "最大(営)", "net")
	for _, b := range rep.Buckets {
		fmt.Printf("%-24s %-20s %4d %8.2f %8.2f %8.2f %8.0f %8.0f %8.0f %13.0f\n",
			b.Strategy, b.CloseReason, b.N, b.MedianDays, b.P90Days, b.MaxDays,
			b.MedianBusinessDays, b.P90BusinessDays, b.MaxBusinessDays, b.NetJPY)
	}
	for _, w := range rep.Warnings {
		fmt.Printf("\n⚠ %s\n", w)
	}
	// 🛑 決済時の peak / trough の分布。トレールの床は max(arm, peak − giveback) なので、
	// 「armed に届いたか」「届いてからどれだけ返したか」はここでしか読めない。
	fmt.Printf("\n── 決済時の MFE / MAE(円/株)──\n")
	fmt.Printf("%-24s %-20s %4s %10s %10s %10s %14s\n",
		"戦略", "決済理由", "N", "peak中央", "peak p90", "trough中央", "armed/トレール")
	for _, b := range rep.Buckets {
		armed := "–"
		if b.RatchetN > 0 {
			armed = fmt.Sprintf("%d/%d", b.ArmedN, b.RatchetN)
		}
		fmt.Printf("%-24s %-20s %4d %10.1f %10.1f %10.1f %14s\n",
			b.Strategy, b.CloseReason, b.N, b.MedianPeakJPY, b.P90PeakJPY, b.MedianTroughJPY, armed)
	}
	fmt.Println("🛑 armed は **peak >= ratchet_arm** で数える(決済理由で数えると、armed に届いたのに")
	fmt.Println("   SL / max_hold で出た建玉が落ちる)。「–」= トレールを持たない戦略。")

	fmt.Println("\n🛑 (暦)= 暦日(休場を含む)/ (営)= **営業日**。MaxHold は営業日で切られているので、")
	fmt.Println("   「期限にぶつかったか」を読むのは**営業日の側**。暦日だけ見ると週末ぶん水増しされ、")
	fmt.Println("   7営業日の atr_breakout_v2 が「9日持った」ように見える。")
	fmt.Println("🛑 手仕舞い(manual / forced_flat)は自然な出口ではない — 分布に混ぜて読まない。")
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "holding-period:", err)
	os.Exit(1)
}
