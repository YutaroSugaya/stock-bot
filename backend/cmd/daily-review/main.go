// Command daily-review は日次総評の**段1 = 決定論パケット**。
// その日の数字を JSON にして ~/.stockbot/journal/YYYY-MM-DD.json へ書く。
//
// READ-ONLY: SELECT と CSV 読みだけ。発注 API も書込 SQL もこのバイナリに型として
// 存在しない(cmd/forward-report と同じ規律)。
//
// 🛑 **これは日記であってエッジの証拠ではない。** 撤退・継続・ロットの判断には使わない
// — 基準は事前にコミットした基準、判定は cmd/edge-judge が出す。
// 段2(LLM が文章を書く)はこの JSON **だけ**を入力にする。数字を LLM に作らせない。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"stockbot/backend/internal/adapter/repository/pg"
	"stockbot/backend/internal/app/journal"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
)

// packet は段2 へ渡す全体。query の DailyJournal(台帳側)に、市況とヘルスを足す。
func main() {
	dateStr := flag.String("date", "", "対象日(JST の YYYY-MM-DD。空 = 今日)")
	db := flag.String("db", "research", "どちらの台帳を読むか: research(STOCKBOT_DATABASE_URL)| harvest(STOCKBOT_HARVEST_DATABASE_URL)| live(STOCKBOT_LIVE_DATABASE_URL)")
	dataDir := flag.String("data", "data", "<sym>_daily.csv と bench_topix.csv を置いたディレクトリ")
	// 🚨 **既定は `-date` に追従する**。以前は無条件に `today.txt` を指していたので、
	// 過去日を backfill すると **その日に見ていた 200 銘柄ではなく今日の 200 銘柄**で
	// 市況(universe_n / 上昇 / 下落 / 中央値)を組み、段2 は O_EXCL なので
	// **その誤った数字が永久に固定される**。
	// bot 側の引け後ジョブは最初から archive を見ているのに、その関数のエラー文が
	// 案内する手動コマンドだけが today.txt を見ていた。
	universe := flag.String("universe", "", "その日のユニバースファイル(空 = -date に応じて自動: 今日なら today.txt、過去日なら universe/archive/<date>.txt)")
	// 🛑 既定は **-db に追従**する(下で解決)。research と live を同じディレクトリに
	// 書くとファイル名が日付なので**片方が上書きで消える** — bot 側の引け後ジョブは
	// live を ~/.stockbot/journal/live/ に分けているので、手動経路も揃える。
	outDir := flag.String("out", "", "JSON の出力先ディレクトリ(空 = -db に応じて既定: research は ~/.stockbot/journal、live は その live/ サブディレクトリ)")
	stdout := flag.Bool("stdout", false, "ファイルに書かず stdout へ出す")
	// 段2(LLM が文章を書く)。既定 OFF — 段1 だけでも毎日の記録として価値があり、
	// 同時に走らせると失敗時に「LLM のせいか集計のせいか」を切り分けられなくなる。
	stage2 := flag.Bool("stage2", false, "段1 の JSON を claude CLI に渡して総評(.md)も書く")
	claudeCLI := flag.String("claude", "claude", "claude CLI のパス(段2 用)")
	// 10分では足りない: 同じ `--effort max` を使う advisor の実測は 621秒(bot 側の
	// stage2PerDayTimeout と同じ理由)。短く切ると毎回 timeout する。
	stage2Timeout := flag.Duration("stage2-timeout", 30*time.Minute, "段2 の上限時間")
	flag.Parse()

	if *outDir == "" {
		*outDir = defaultOutDir(*db)
	}

	day := clock.System()()
	if *dateStr != "" {
		t, err := time.ParseInLocation("2006-01-02", *dateStr, clock.JST)
		if err != nil {
			fatal(fmt.Errorf("-date は YYYY-MM-DD: %w", err))
		}
		day = t
	}

	url, err := config.LedgerDSN(*db)
	if err != nil {
		fatal(err)
	}
	ctx := context.Background()
	pool, err := pg.Open(ctx, url)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()

	uni := *universe
	if uni == "" {
		uni = journal.UniversePathFor(day, time.Now())
	}
	p, err := journal.Build(ctx, pg.NewJournalRepo(pool), day, *dataDir, uni)
	if err != nil {
		fatal(err)
	}
	ledger := p.Ledger
	blob, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		fatal(err)
	}
	if *stdout {
		os.Stdout.Write(append(blob, '\n'))
		return
	}
	// 🛑 **空になった測り直しで上書きしない**(期間の境界での DSN 取り違え)。
	// この安全弁は bot 側にしか無く、**bot のエラー文が案内するこのコマンド**が
	// 素通りする。共有の journal パッケージに置いて両経路で通す。
	if err := journal.RefuseEmptyRebuild(*outDir, p.Ledger.Date, p); err != nil {
		fatal(err)
	}
	path, blob2, err := journal.WriteJSON(*outDir, p)
	if err != nil {
		fatal(err)
	}
	blob = blob2
	fmt.Printf("── daily-review(段1・決定論パケット)── %s\n", ledger.Date)
	fmt.Printf("  決済 %d本(勝 %d / 負 %d / 引分 %d)net %.0f円 = gross %.0f − fee %.0f + carry %.0f\n",
		ledger.Trades.N, ledger.Trades.Wins, ledger.Trades.Losses, ledger.Trades.Flat,
		ledger.Trades.NetJPY, ledger.Trades.GrossJPY, ledger.Trades.FeeJPY, ledger.Trades.CarryJPY)
	fmt.Printf("  当日建玉 %d本 / 引け時点の建玉 %d本 / 見送り理由 %d種\n", len(ledger.Entries), ledger.Open.N, len(ledger.Rejections))
	if p.Market.Unavailable != "" {
		fmt.Printf("  市況: %s\n", p.Market.Unavailable)
	} else {
		fmt.Printf("  市況: ユニバース %d銘柄 上昇 %d / 下落 %d(中央値 %+.2f%%)\n",
			p.Market.UniverseN, p.Market.Advancing, p.Market.Declining, p.Market.MedianPct)
	}
	fmt.Printf("  → %s\n", path)
	fmt.Println("  ⚠ 日記であってエッジの証拠ではない(判定は cmd/edge-judge)")

	if *stage2 {
		md, err := journal.RunStage2(ctx, *claudeCLI, *outDir, ledger.Date, blob, *stage2Timeout)
		if err != nil {
			// 段2 が落ちても段1 は残っている。**数字は失われない**ので exit 0 のまま
			// 警告だけ出す。既に .md がある日の再実行(段1 の測り直し)も同じ経路に
			// 来るので、ここで exit 1 にすると良性の再実行が job 失敗になる。
			fmt.Fprintln(os.Stderr, "daily-review: 段2 は書けなかった(段1 の JSON は残っている):", err)
			return
		}
		fmt.Printf("  → %s(段2)\n", md)
	}
}

// defaultOutDir は台帳ごとの既定出力先。**live は必ず別ディレクトリ**(ファイル名が
// 日付なので同居させると上書きで片方が消える)。cmd/stockbot の journalDirForTrack と
// 同じ規約。
// defaultOutDir は日次パケットの出力先。**トラックごとに分ける** —— ファイル名は
// 日付なので、2 トラックを同じディレクトリに書くと**片方が上書きで消える**。
//
// 🚨 `-db harvest` を足したとき、ここを広げ忘れると旧台帳が research の枝に落ちる。
// `WriteJSON` は素の `os.WriteFile` なので、
// 旧台帳の同じ日を書いた瞬間に **research のその日のパケットが消える** —— しかも
// `.md` は O_EXCL なのでその日の段2 はもう使い切られている。
// 「track を足したら出力先も足す」を忘れないよう、既定は**明示的な表**にする。
func defaultOutDir(db string) string {
	switch db {
	case "live":
		return filepath.Join(journal.DefaultDir(), "live")
	case "harvest":
		return filepath.Join(journal.DefaultDir(), "harvest")
	default: // research(既定)
		return journal.DefaultDir()
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "daily-review:", err)
	os.Exit(1)
}
