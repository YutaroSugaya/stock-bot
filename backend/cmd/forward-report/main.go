// Command forward-report summarises the closed-trade ledger (trades) — 日足統一後、
// forward 記録が唯一のエッジ証拠。その読み出し面。
//
// READ-ONLY: SELECT だけ。発注 API も書込 SQL もこのバイナリに型として存在しない。
// エッジ判定そのものはしない — 判定は事前にコミットした基準と
// cmd/edge-judge に委ね、途中経過で裁量判断しない。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"stockbot/backend/internal/adapter/repository/pg"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/usecase/query"
)

func main() {
	sinceStr := flag.String("since", "", "この JST 日付以降の closed trade だけ集計(YYYY-MM-DD。空 = 全期間)")
	asJSON := flag.Bool("json", false, "集計を JSON で stdout へ(net_pnl_jpy は cmd/edge-judge -in にそのまま渡せる)")
	last := flag.Int("last", 10, "人間向け表示で出す直近トレード数")
	stratFilter := flag.String("strategy", "", "この戦略の trade だけに絞る(edge-judge に掛ける単位。空 = 全戦略)")
	sideFilter := flag.String("side", "", "BUY / SELL のどちらかだけに絞る(売り側 edge-judge。空 = 両方)")
	maxNotional := flag.Float64("max-notional-jpy", 0, "建玉金額(建値×株数)がこの円以下の trade だけに絞る資金キャパシティ分計(0 = 無制限・-strategy と併用可)")
	// hybrid: 台帳は track ごとに**別 DB**(物理分離)。live の forward 記録 =
	// 実弾の唯一のエッジ証拠なので、読み出し口が無いと台帳に載せられない。
	db := flag.String("db", "research", "どちらの台帳を読むか: research(STOCKBOT_DATABASE_URL)| harvest(STOCKBOT_HARVEST_DATABASE_URL)| live(STOCKBOT_LIVE_DATABASE_URL)")
	flag.Parse()

	url, err := config.LedgerDSN(*db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forward-report:", err)
		os.Exit(2)
	}
	var since time.Time
	if *sinceStr != "" {
		t, err := time.ParseInLocation("2006-01-02", *sinceStr, clock.JST)
		if err != nil {
			fatal(fmt.Errorf("-since は YYYY-MM-DD: %w", err))
		}
		since = t
	}

	ctx := context.Background()
	pool, err := pg.Open(ctx, url)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()

	// score resolver: エントリー時にその戦略の screener が出していた score を
	// advisor_runs から復元(migration 0007 の advisor_run_id 直参照 + 旧行の
	// 時刻 fallback。復元不能は黙って落とさず件数を出す)。
	opts := []query.ForwardReportOption{
		query.WithStrategyResolver(pg.NewPositionRepo(pool)),
		query.WithScoreResolver(pg.NewScoreRepo(pool)),
	}
	if *stratFilter != "" {
		opts = append(opts, query.WithStrategyFilter(*stratFilter))
	}
	if *sideFilter != "" {
		// 🛑 **綴り誤りを fail-close で落とす**。通すと「該当ゼロ」が返り、
		// 「その向きの取引が無かった」と読まれる(絞ったこと自体が消える)。
		switch strings.ToUpper(strings.TrimSpace(*sideFilter)) {
		case "BUY", "SELL":
		default:
			fatal(fmt.Errorf("-side は BUY か SELL: %q", *sideFilter))
		}
		opts = append(opts, query.WithSideFilter(*sideFilter))
	}
	if *maxNotional > 0 {
		opts = append(opts, query.WithMaxNotionalJPY(*maxNotional))
	}
	rep, err := query.NewBuildForwardReport(pg.NewTradeRepo(pool), opts...).Execute(ctx, since)
	if err != nil {
		fatal(err)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		// err を捨てない: 短い書き込み(ディスク満杯 / パイプ切断)で**切り詰められた
		// JSON を成功終了で返す**と、edge-judge がパースに失敗するまで気づけない。
		// cmd/edge-judge と同じ理由。
		if err := enc.Encode(rep); err != nil {
			fatal(err)
		}
		return
	}

	period := "全期間"
	if rep.Since != "" {
		period = rep.Since + " 以降"
	}
	// フィルタ中である事実は必ず見せる: typo した戦略名が「trade ゼロ」に化けて
	// 全体の話と読み違える事故を防ぐ(実弾昇格の判断材料を読む画面なので)。
	filterNote := ""
	if rep.StrategyFilter != "" {
		filterNote = fmt.Sprintf("  戦略フィルタ: %s", rep.StrategyFilter)
	}
	// 🚨 **向きで絞った事実も必ず見せる**。ここが抜けると、
	// `-side SELL` の数字が全件だと読まれる / `-side` の typo が「trade ゼロ」に化ける
	// 経路が開く。
	if rep.SideFilter != "" {
		filterNote += fmt.Sprintf("  向き: %s のみ(売り側 judge。診断であって昇格ゲートではない)", rep.SideFilter)
	}
	// 資金分計中である事実も必ず見せる — 部分集合の数字を全体の数字と読み違えない。
	if rep.MaxNotionalJPY > 0 {
		filterNote += fmt.Sprintf("  建玉≤%.0f円(資金分計・診断であって昇格ゲートではない)", rep.MaxNotionalJPY)
	}
	fmt.Printf("── forward-report(trades 台帳・net = gross − fee + carry)──\n")
	if rep.N == 0 {
		fmt.Printf("期間: %s%s / closed trade はまだありません。\n", period, filterNote)
		// 🛑 **ここでこそ出す**。全件が巻き戻しだった日は「何も起きなかった」と
		// 「実弾が動いたが戦略の出口ではなかった」が区別できないと台帳が嘘になる。
		if rep.CompensatedN > 0 {
			fmt.Printf("  ⚠ ただし entry_compensated が %d 件あります(約定後に守りを置けず巻き戻した往復)— 実弾は動いていますが戦略の出口ではないので N には入れていません\n", rep.CompensatedN)
		}
		if rep.ExternalN > 0 {
			fmt.Printf("  ⚠ ただし external_close が %d 件あります(人間が建てた建玉の決済)— 口座には効いていますが戦略の出口ではないので N には入れていません\n", rep.ExternalN)
		}
		noteCountingSplit(rep)
		if rep.StrategyFilter != "" {
			fmt.Printf("(この戦略名の trade が 0 件 — 戦略名の綴りも確認: bnf_reversion / bnf_reversion_trail / post_jump_drift / high_volume_premium / bnf_intraday_reversion)\n")
		} else {
			fmt.Printf("(シグナルが無い週があるのは正常 — BNF はパニック月に集中する。判定を急がない)\n")
		}
		return
	}
	fmt.Printf("期間: %s%s  N=%d  勝ち %d (%.1f%%)  net %+.0f JPY(gross %+.0f / fee %.0f / carry %+.0f)  銘柄数 %d\n",
		period, filterNote, rep.N, rep.Wins, float64(rep.Wins)/float64(rep.N)*100,
		rep.NetTotalJPY, rep.GrossTotalJPY, rep.FeeTotalJPY, rep.CarryTotalJPY, rep.SymbolN)
	// 判定に使うのは正規化した方(建玉金額の差を消す)。生の円は口座に効いた実額。
	fmt.Printf("¥1M notional 正規化: net %+.0f JPY / 1本あたり %+.0f  ← edge-judge はこちらを判定する\n",
		rep.NetPer1MTotalJPY, rep.NetPer1MTotalJPY/float64(rep.N))
	// 黙って除外しない。戦略の出口ではない往復なので N には入れないが、実弾が動いた
	// 事実は画面に残す。
	if rep.CompensatedN > 0 {
		fmt.Printf("  ⚠ エッジ標本から除外 %d 件(close_reason=entry_compensated = 約定後に守りを置けず巻き戻した往復)— 戦略の出口ではないので N・net に入れていない\n",
			rep.CompensatedN)
	}
	if rep.ExternalN > 0 {
		fmt.Printf("  ⚠ エッジ標本から除外 %d 件(close_reason=external_close = 人間が建てた建玉の決済)— 口座には効いているが戦略の出口ではない\n",
			rep.ExternalN)
	}
	noteCountingSplit(rep)
	fmt.Printf("月次:\n")
	for _, m := range rep.ByMonth {
		fmt.Printf("  %s  N=%-3d  net %+.0f\n", m.Month, m.N, m.NetJPY)
	}
	if len(rep.ByStrategy) > 0 {
		fmt.Printf("戦略別(net 降順 — 実弾判断はこの分計と edge-judge で):\n")
		for _, s := range rep.ByStrategy {
			fmt.Printf("  %-24s N=%-3d 勝ち %-3d net %+.0f  (¥1M正規化 %+.0f)\n", s.Strategy, s.N, s.Wins, s.NetJPY, s.NetPer1MJPY)
			// 🛑 **売りが 1 本でもあれば向きの内訳を出す**。
			// `direction: both` の 8 アームは買いと売りを同じ net に合算するので、
			// 「買いで勝って売りで負けている」が平らに見える。売りゼロの戦略で
			// 毎行出すと画面が埋まるだけなので、出すのは混ざっている戦略だけ。
			if s.SellN > 0 {
				fmt.Printf("      向き別:  買 n=%d net %+.0f   **売 n=%d net %+.0f**  ← 事前登録は売りを別の問いとして測る\n",
					s.BuyN, s.BuyNetJPY, s.SellN, s.SellNetJPY)
			}
			// 🛑 **決済理由ごとの本数と net**。JSON にしか無いと、人間が読む面では
			// 「MaxHold で閉じた分が戦略ごとにいくらか」= 期限が効いたか効きすぎたかを
			// **確かめようが無い**。
			if len(s.ByReason) > 0 {
				fmt.Printf("      理由別:")
				for _, br := range s.ByReason {
					fmt.Printf("  %s n=%d net %+.0f(/1M %+.0f)", br.Reason, br.N, br.NetJPY, br.NetPer1MJPY)
				}
				fmt.Println()
			}
			// score 分位は**戦略内**でのみ切る(score は戦略ごとに定義が違い、
			// 横断で並べると交絡する — abs_momentum は上限なし、donchian は 1.0 付近が上限)。
			if sv := s.Score; sv != nil {
				if sv.Restored > 0 {
					fmt.Printf("      score: 復元 %d/%d  中央値 %.2f  低位 n=%-2d net/1M %+.0f  高位 n=%-2d net/1M %+.0f\n",
						sv.Restored, sv.Restored+sv.Missing, sv.Median, sv.LowN, sv.LowAvgPer1MJPY, sv.HighN, sv.HighAvgPer1MJPY)
				} else {
					fmt.Printf("      score: 復元 0/%d(この戦略は全件復元不能)\n", sv.Missing)
				}
			}
		}
		if rep.ScoreMissingN > 0 {
			fmt.Printf("  ⚠ score 復元不能 %d 件(60秒窓に run 無し・手動 config 等)— 分位集計は復元できた分のみで、黙って除外はしていない\n", rep.ScoreMissingN)
		}
	}
	fmt.Printf("理由別:")
	reasons := make([]string, 0, len(rep.ByReason))
	for reason := range rep.ByReason {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		fmt.Printf("  %s=%d", reason, rep.ByReason[reason])
	}
	fmt.Println()
	from := len(rep.Trades) - *last
	if from < 0 {
		from = 0
	}
	fmt.Printf("直近 %d 件:\n", len(rep.Trades)-from)
	for _, tr := range rep.Trades[from:] {
		fmt.Printf("  %s  %-5s %-22s %-4s %5d株  %.1f→%.1f  net %+8.0f  %s\n",
			tr.ClosedAt, tr.Symbol, tr.Strategy, tr.Side, tr.Quantity, tr.EntryPrice, tr.ClosePrice, tr.NetJPY, tr.CloseReason)
	}
	fmt.Printf("──────────────────────────────────────────────\n")
	fmt.Printf("注意: エッジの合否はここでは出さない。撤退/継続は事前にコミットした基準で。\n")
}

// noteCountingSplit は「ダッシュボードと数字が合わない」を**バグと読ませない**ための
// 一行。画面は口座ベース(entry_compensated / external_close も戦績に
// 計上)で、ここはエッジ標本(除外)。同じ台帳から違う N が出るのは意図であり、
// どちらの数え方かは JSON の `counting` が名乗る。
func noteCountingSplit(rep query.ForwardReportView) {
	if rep.CompensatedN == 0 && rep.ExternalN == 0 {
		return
	}
	fmt.Printf("    (ダッシュボードの戦績は**口座ベース**でこの %d 件も計上しています — N が食い違うのは仕様です)\n",
		rep.CompensatedN+rep.ExternalN)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "forward-report:", err)
	os.Exit(1)
}
