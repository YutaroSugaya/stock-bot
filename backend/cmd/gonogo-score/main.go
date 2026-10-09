// Command gonogo-score は寄り前の go/no-go の採点(READ-ONLY)。
//
// (日付, 銘柄)で判定(~/.stockbot/gonogo/*.jsonl)と paper の bnf 家族の建玉(その日に建って決済したもの・
// 戦略の出口だけ)を結合し、go / no_go / unknown / late / 未判定 の群で net を比べる(¥1M 正規化・day-block
// bootstrap)。使う判定は建った時刻より前の最後の成功行だけで、建った後の判定しか無い建玉は late に分ける。live で人間が止めた銘柄は、止めていた間の paper の結果を別表に出す(止めた判断の答え合わせ)。
//
// 🛑 判定は後からまとめて読む。差が出るまで LLM に自動の拒否権は持たせない(CLAUDE.md §4)。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	adapter "stockbot/backend/internal/adapter/gonogo"
	"stockbot/backend/internal/adapter/repository/pg"
	"stockbot/backend/internal/app/gonogo"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

func main() {
	home, _ := os.UserHomeDir()
	sinceStr := flag.String("since", "2026-10-02", "この JST 日付以降の判定と建玉(記録の開始は 2026-10-02)")
	db := flag.String("db", "research", "建玉を読む台帳(research = STOCKBOT_DATABASE_URL)")
	dir := flag.String("dir", filepath.Join(home, ".stockbot", "gonogo"), "判定ファイルの置き場")
	blocksLog := flag.String("blocks-log", "", "live の銘柄停止の操作の記録(空 = STOCKBOT_LIVE_SYMBOL_BLOCKS / live の emergency フラグと同じディレクトリ)")
	version := flag.String("prompt-version", "", "採点する判定の版(空 = 1 つしか無いときだけ)")
	resamples := flag.Int("resamples", 10000, "bootstrap のリサンプル回数")
	seed := flag.Int64("seed", 42, "bootstrap の seed")
	flag.Parse()

	since, err := time.ParseInLocation("2006-01-02", *sinceStr, clock.JST)
	if err != nil {
		fatal(fmt.Errorf("-since は YYYY-MM-DD: %w", err))
	}
	judgments, err := readJudgments(*dir, *sinceStr)
	if err != nil {
		fatal(err)
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
	rows, err := pg.NewPairRepo(pool).ListPairTrades(ctx, since)
	if err != nil {
		fatal(err)
	}
	var trades []gonogo.ScoredTrade
	for _, r := range rows {
		if r.OpenedAt.Before(since) {
			continue
		}
		trades = append(trades, gonogo.ScoredTrade{Symbol: r.Symbol, Strategy: r.Strategy, EntryPrice: r.EntryPrice,
			Quantity: r.Quantity, NetJPY: r.NetJPY, OpenedAt: r.OpenedAt})
	}
	rep, err := gonogo.Score(judgments, trades, *version, *resamples, *seed)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("寄り前 go/no-go の採点(%s 以降・判定 %d 行・版 %s・paper の bnf 家族・決済済み・戦略の出口だけ)\n",
		*sinceStr, len(judgments), rep.PromptVersion)
	fmt.Printf("%-9s %4s %5s %12s %14s %12s  %s\n", "群", "N", "日数", "net", "net/1M 合計", "1 本/1M", "95% CI(day-block)")
	for _, g := range rep.Groups {
		fmt.Printf("%-9s %4d %5d %+12.0f %+14.0f %+12.0f  [%+.0f, %+.0f]\n",
			g.Name, g.N, g.DayBlocks, g.NetJPY, g.NetPer1MJPY, g.MeanPer1MJPY, g.CILo, g.CIHi)
	}
	if len(rep.NoGoByCategory) > 0 {
		fmt.Println("  no_go の内訳(カテゴリ別・構造的な悪材料と一時的な悪材料を分けて読む):")
		for _, c := range rep.NoGoByCategory {
			fmt.Printf("  %-24s %4d %5d %+12.0f %+14.0f %+12.0f  [%+.0f, %+.0f]\n",
				c.Name, c.N, c.DayBlocks, c.NetJPY, c.NetPer1MJPY, c.MeanPer1MJPY, c.CILo, c.CIHi)
		}
	}
	fmt.Println("  ※ 判定は建つ前の最後の判定。late = 建った後の判定しか無い(後出しなので群に入れない)。")
	fmt.Println("  ※ 判定は締めで読む。day_blocks < 20 の群は判定しない(EDGE_METHODOLOGY)。")

	path := *blocksLog
	if path == "" {
		path = strings.TrimSuffix(config.LoadLiveTrackEnv().SymbolBlocksFile(), ".json") + ".log"
	}
	ops, err := readBlockOps(path)
	if err != nil {
		fatal(err)
	}
	bt := gonogo.BlockedTrades(ops, trades, time.Now())
	fmt.Printf("\nlive で人間が止めていた間の paper の bnf 家族(%s・%d 本)\n", path, len(bt))
	for _, b := range bt {
		fmt.Printf("  %s %s %-30s 止めた %s  net %+.0f  %s\n", b.Trade.OpenedAt.In(clock.JST).Format("2006-01-02"),
			b.Trade.Symbol, b.Trade.Strategy, b.BlockedAt.In(clock.JST).Format("01-02 15:04"), b.Trade.NetJPY, b.Note)
	}
}

// readJudgments は since 以降の判定ファイルの全行を書かれた順に読む(建つ前の判定を選ぶのは gonogo.Score)。
func readJudgments(dir, since string) ([]port.GoNoGoRecord, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	j := &adapter.Journal{Dir: dir}
	var out []port.GoNoGoRecord
	for _, p := range paths {
		date := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		if date < since {
			continue
		}
		rs, err := j.Read(date)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// readBlockOps は live_symbol_blocks.log(JSONL)を読む。無ければ空。
func readBlockOps(path string) ([]gonogo.BlockOp, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []gonogo.BlockOp
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e struct {
			At, Symbol, Action, Note string
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, e.At)
		if err != nil {
			continue
		}
		out = append(out, gonogo.BlockOp{At: at, Symbol: e.Symbol, Action: e.Action, Note: e.Note})
	}
	return out, sc.Err()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gonogo-score:", err)
	os.Exit(1)
}
