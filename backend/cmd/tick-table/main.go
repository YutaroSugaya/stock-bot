// Command tick-table regenerates the 細かい呼値 symbol table from observed prices.
//
// READ-ONLY apart from ONE generated Go file. It reads hard_limits.allowed_symbols
// (the pool) and the local <sym>_daily.csv bars, asks market.InferTickRegimeFromBars which
// 呼値 table each symbol trades on, and writes internal/domain/market/tick_fine_gen.go.
//
// なぜ生成なのか: 手で並べた表は**ユニバースが入れ替わるたびに穴が空く**。日次
// ユニバースは毎朝プール 1,551 から 200 を選び直すので、表が旧ユニバースぶんしか
// 無いと新規採用銘柄が丸ごと粗いテーブルへ倒れ、その銘柄だけ紙執行のコストが
// 最大10倍で記録される。
//
// 判定は証拠ベースで、証拠が無ければ粗いテーブル(fail-safe)。したがって
// データが薄い銘柄を Fine に誤昇格させて刻み違反の注文を作ることはない。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"

	"stockbot/backend/internal/adapter/candlecsv"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
)

func main() {
	dir := flag.String("data", "data", "<sym>_daily.csv を置いたディレクトリ")
	allowFile := flag.String("allow-file", "../configs/hard_limits.yaml",
		"hard_limits.yaml のパス。allowed_symbols がプール(走査対象)になる")
	out := flag.String("out", "internal/domain/market/tick_fine_gen.go", "生成先")
	check := flag.Bool("check", false, "生成せず、既存ファイルとの差分の有無だけ報告する(差分検査用)")
	verbose := flag.Bool("verbose", false, "銘柄ごとの判定を出す")
	flag.Parse()

	if err := run(*dir, *allowFile, *out, *check, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "tick-table:", err)
		os.Exit(1)
	}
}

func run(dir, allowFile, out string, check, verbose bool) error {
	hl, err := config.LoadHardLimits(allowFile)
	if err != nil {
		return fmt.Errorf("hard_limits: %w", err)
	}
	pool := append([]string(nil), hl.AllowedSymbols...)
	if len(pool) == 0 {
		return fmt.Errorf("allowed_symbols が空 — プールを読めていない(fail-close)")
	}
	sort.Strings(pool)

	var fine, missing []string
	bars := 0
	for _, sym := range pool {
		cs, err := candlecsv.LoadDaily(dir, sym)
		if err != nil || len(cs) == 0 {
			missing = append(missing, sym)
			continue
		}
		bars += len(cs)
		// 四本値すべてを証拠に使い(終値だけだと偶然グリッドに乗る確率が上がる)、
		// **証拠の鮮度**も見る。CSV は ChainLinkSplits 適用後なので、分割前のバーは
		// raw/N になって粗いグリッドから外れる — それを Fine の証拠と読むと 1円刻みの
		// 銘柄に 0.5 刻みの守りを作って broker に拒否される(実測で 93 銘柄)。
		r := market.InferTickRegimeFromBars(cs)
		if r == market.TickRegimeFine {
			fine = append(fine, sym)
		}
		if verbose {
			fmt.Printf("%-6s %-6s bars=%d\n", sym, r, len(cs))
		}
	}

	in := renderInput{
		Fine:       fine,
		PoolSize:   len(pool),
		PoolDigest: poolDigest(pool),
		DerivedOn:  clock.System()().Format("2006-01-02"),
		// ローカルの絶対パスを生成物に焼き込まない(公開リポに個人環境が写る)。
		DataNote: fmt.Sprintf("%s の日足 %d 本(四本値すべてを証拠に使用)・日足が無い銘柄 %d 件は証拠なし=粗いテーブル",
			filepath.Base(dir), bars, len(missing)),
	}
	if err := validate(in); err != nil {
		return err
	}

	src, err := format.Source(render(in))
	if err != nil {
		return fmt.Errorf("生成コードが gofmt を通らない: %w", err)
	}

	fmt.Printf("プール %d 銘柄 → 細かい呼値 %d / 粗い %d(うち日足なし %d)\n",
		len(pool), len(fine), len(pool)-len(fine), len(missing))
	if len(missing) > 0 {
		n := len(missing)
		if n > 10 {
			n = 10
		}
		fmt.Printf("  ⚠ 日足が無く判定できなかった銘柄(粗い側へ倒す): %v%s\n",
			missing[:n], map[bool]string{true: " …"}[len(missing) > 10])
	}

	if check {
		old, err := os.ReadFile(out)
		if err != nil {
			return fmt.Errorf("%s が読めない(-check): %w", out, err)
		}
		// 導出日の行だけは毎回変わるので比較から外す(内容の差分だけ見る)。
		if bytes.Equal(stripDerivedOn(old), stripDerivedOn(src)) {
			fmt.Println("差分なし — 再生成は不要")
			return nil
		}
		return fmt.Errorf("%s が最新でない。go run ./cmd/tick-table -allow-file %s で再生成して diff を人間が読むこと", out, allowFile)
	}

	if err := os.WriteFile(out, src, 0o644); err != nil {
		return fmt.Errorf("書き込み: %w", err)
	}
	fmt.Printf("生成: %s (digest %s)\n", out, in.PoolDigest)
	return nil
}

// stripDerivedOn removes the derivation-date line so -check compares content only.
func stripDerivedOn(src []byte) []byte {
	var keep [][]byte
	for _, ln := range bytes.Split(src, []byte("\n")) {
		if bytes.Contains(ln, []byte("FineTickDerivedOn")) || bytes.Contains(ln, []byte("導出日")) {
			continue
		}
		keep = append(keep, ln)
	}
	return bytes.Join(keep, []byte("\n"))
}
