// Command fetch-daily refreshes the daily-candle CSVs from the 立花 e支店 API.
// 参照系のみ(発注 API はこのファイルに型として存在しない)。取得0本 / API エラー /
// ガード拒否のいずれでも既存 CSV は無傷。推奨運用は翌朝(寄り前)の実行。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"stockbot/backend/internal/adapter/apiusage"
	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/candlecsv"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

func main() {
	out := flag.String("out", "data", "日足 CSV のディレクトリ(<sym>_daily.csv)")
	symbolsCSV := flag.String("symbols", "", "対象銘柄(カンマ区切り)。空 = -out にある既存 CSV 全部")
	bars := flag.Int("bars", 250, "取得する日足本数(立花の遡及上限に依存)")
	rebuild := flag.Bool("rebuild", false, "既存 CSV を退避して立花のデータだけで作り直す")
	archiveDir := flag.String("archive", "legacy_mixed", "-rebuild 時の退避先(-out からの相対)")
	includeToday := flag.Bool("include-today", false, "当日(JST)のバーも取り込む(夜間バッチ前の未確定足が固定されるリスクを承知の上で)")
	benchSymbol := flag.String("bench-symbol", "", "TOPIX ベンチマークとして bench_topix.csv に継ぎ足す立花銘柄コード(空 = bench を更新しない)")
	// CSV は git 管理外 + pg_dump の対象外で、立花の遡及(≈250本)を超えた日は
	// 再取得できない = 唯一のコピー。空にするのは明示的 opt-out(通常運用ではしない)。
	backupDir := flag.String("backup-dir", defaultBackupDir(), "書き換え前の退避先(空 = 退避しない)")
	backupKeep := flag.Int("backup-keep", 30, "退避の保持世代数")
	// -check は立花 API に一切繋がない。launchd 下で macOS TCC によりデータが
	// 読めるかを、場中でも(bot のセッションを蹴らずに)確かめるために要る。
	checkOnly := flag.Bool("check", false, "取得せず、対象銘柄と CSV の日付範囲だけ報告する(TCC / パス確認用)")
	// 逃げ道は 1 つだけ。**朝が途中で落ちた日の引き直し**が唯一の正当な用途で、
	// 通常運用では打たない(打つと当日枠を 1,550 回ぶん余計に食う)。
	ignoreBudget := flag.Bool("ignore-budget", false, "当日枠 / 二重取得のガードを外す(朝が途中で落ちた日の引き直し用)")
	flag.Parse()

	symbols, err := targetSymbols(*out, *symbolsCSV)
	if err != nil {
		log.Fatalf("対象銘柄の解決に失敗: %v", err)
	}
	if len(symbols) == 0 {
		log.Fatalf("対象銘柄がありません(-symbols を指定するか %s に <sym>_daily.csv を置く)", *out)
	}

	// 🛑 **backup と login より前に検問する**。拒否するなら tar.gz(1,549 CSV)も
	// login も無駄で、`-check` は API を叩かないのでガードの対象外。
	// カウンタは login より前に開く(最初の login も broker 側から見れば 1 リクエスト)。
	usage := apiusage.Open(apiusage.DefaultDir(), "fetch-daily", nil)
	if !*checkOnly {
		if err := enforceRequestBudget(budgetInputs{
			u: usage.Snapshot(), symbols: len(symbols), bench: *benchSymbol != "",
			wholePool: *symbolsCSV == "", ignore: *ignoreBudget,
		}); err != nil {
			log.Fatalf("取得を中止します(CSV は無傷): %v", err)
		}
	}

	// 退避は書き込みの前に fail-close。日足が古いのは復旧可能(次回の取得で追いつく)、
	// CSV が壊れるのは復旧不能。
	if *backupDir != "" {
		path, err := backupDailyCSVs(*out, *backupDir, time.Now(), *backupKeep)
		if err != nil {
			log.Fatalf("退避に失敗したので取得を中止します(CSV は無傷): %v", err)
		}
		if path != "" {
			fmt.Printf("退避: %s\n", path)
		}
	}

	if *checkOnly {
		oldest, newest := dailyDayRange(*out, symbols)
		fmt.Printf("check: %d 銘柄 / CSV 日付 %s 〜 %s / dir=%s\n",
			len(symbols), orDash(oldest), orDash(newest), *out)
		return
	}

	feed, err := newFeed()
	if err != nil {
		log.Fatalf("立花 adapter の構築に失敗: %v", err)
	}
	attachUsageRecorder(feed, usage)
	ctx := context.Background()
	if err := feed.RefreshToken(ctx); err != nil {
		log.Fatalf("login 失敗: %v", err)
	}

	o := fetchOpts{bars: *bars, includeToday: *includeToday, now: time.Now}
	var updated, skipped, failed int
	for _, sym := range symbols {
		path := candlecsv.DailyFile(*out, sym)
		if *rebuild {
			if err := archive(path, filepath.Join(*out, *archiveDir)); err != nil {
				fmt.Printf("  %s ❌ 退避に失敗(触りません): %v\n", sym, err)
				failed++
				continue
			}
		}
		switch updateFile(ctx, feed, sym, path, o) {
		case outcomeUpdated:
			updated++
		case outcomeSkipped:
			skipped++
		case outcomeFailed:
			failed++
		}
	}
	fmt.Printf("\n完了: 更新 %d / 取得0 %d / 失敗 %d (計 %d 銘柄)\n", updated, skipped, failed, len(symbols))
	// 1 日の API 呼出回数の中で日次最大の塊(1銘柄1リクエスト)。
	// プール拡張で銘柄数が大きく増えても数日気づかれない形になりうるので、
	// 毎回目に入る場所に出す。
	fmt.Printf("立花 API 呼出: %d 回(この実行)\n", apiRequestsOf(feed))
	// 1日の総数は口座単位(立花の集計は 5:30〜翌3:30)。この実行ぶんだけを見ると
	// 「1万以内か」は分からないので、窓の合計も一緒に出す。
	if u := usage.Snapshot(); u.Total > 0 {
		fmt.Printf("本日(%s 起点の開局窓)の口座合計: %d 回 / 上限 %d 回\n",
			u.WindowStart.Format("01-02 15:04"), u.Total, budgetCap)
	}

	if *benchSymbol != "" {
		bo := o
		bo.benchmark = true
		fmt.Printf("\nbenchmark: %s → %s\n", *benchSymbol, benchCSVName)
		updateFile(ctx, feed, *benchSymbol, filepath.Join(*out, benchCSVName), bo)
	}
}

// fetchOpts is the per-run merge policy for updateFile.
type fetchOpts struct {
	bars         int
	benchmark    bool
	includeToday bool
	now          func() time.Time // 「当日」判定の時計(テストで固定する)
}

type outcome int

const (
	outcomeUpdated outcome = iota
	outcomeSkipped
	outcomeFailed
)

// updateFile fetches sym's daily bars and merges them into path (fail-close: 取得
// 失敗・ガード拒否・書き込み失敗のいずれでも既存 CSV は無傷)。
func updateFile(ctx context.Context, feed port.MarketFeed, sym, path string, o fetchOpts) outcome {
	fetched, err := feed.GetKlines(ctx, sym, port.PeriodDaily, o.bars)
	if err != nil {
		fmt.Printf("  %s ❌ 取得失敗(既存 CSV は保持): %v\n", sym, err)
		return outcomeFailed
	}
	fetched, droppedToday := filterFetched(fetched, o.now(), o.includeToday)
	if droppedToday > 0 {
		fmt.Printf("  %s ↳ 当日足 %d 本をスキップ(夜間バッチ前は未確定。翌営業日の実行で確定分が入る)\n", sym, droppedToday)
	}
	if len(fetched) == 0 {
		fmt.Printf("  %s — 取得0本(既存 CSV は保持)\n", sym)
		return outcomeSkipped
	}
	existing, err := loadExisting(path, sym)
	if err != nil {
		fmt.Printf("  %s ❌ 既存 CSV の読み込み失敗(触りません): %v\n", sym, err)
		return outcomeFailed
	}
	var merged []market.Candle
	switch {
	case o.benchmark:
		merged, err = mergeBenchmark(existing, fetched)
	default:
		merged, err = mergeSingleSource(existing, fetched)
	}
	if err != nil {
		fmt.Printf("  %s ⚠ 書き込みスキップ(既存 CSV は保持): %v\n", sym, err)
		return outcomeFailed
	}
	if err := writeCSV(path, merged); err != nil {
		fmt.Printf("  %s ❌ 書き込み失敗: %v\n", sym, err)
		return outcomeFailed
	}
	fmt.Printf("  %s ✅ %d本 (既存 %d + 取得 %d) 最新 %s\n",
		sym, len(merged), len(existing), len(fetched), dayKey(merged[len(merged)-1].OpenTime))
	return outcomeUpdated
}

// filterFetched drops bars dated "today" (JST) unless includeToday。立花の当日足は
// 夜間バッチまで未確定でありうる(引け後すぐの取得では当日バーが返らないことがある)
// 一方、マージは append-only で一度書いた部分足を後から直せない。
func filterFetched(cs []market.Candle, now time.Time, includeToday bool) ([]market.Candle, int) {
	if includeToday {
		return cs, 0
	}
	today := dayKey(now)
	kept := make([]market.Candle, 0, len(cs))
	dropped := 0
	for _, c := range cs {
		if dayKey(c.OpenTime) == today {
			dropped++
			continue
		}
		kept = append(kept, c)
	}
	return kept, dropped
}

// attachUsageRecorder は 1日の総数を数える永続カウンタを立花アダプタへ挿す。
// **朝の 1,551回はここを通さないと誰にも見えない** — bot のプロセス内カウンタは
// 別プロセスなので、挿し忘れるとその分だけ静かに過少になる(立花側の集計と
// 突き合わせると食い違う穴)。立花以外のフィードでは何もしない。
func attachUsageRecorder(feed port.MarketFeed, usage broker.UsageRecorder) {
	if usage == nil {
		return
	}
	if tb, ok := feed.(*broker.Tachibana); ok {
		tb.SetUsageRecorder(usage)
	}
}

// apiRequestsOf reports how many wire requests the feed has sent to 立花 (他の
// フィードは 0 — 表示用で判定には使わない)。
func apiRequestsOf(feed port.MarketFeed) int64 {
	if tb, ok := feed.(*broker.Tachibana); ok {
		return tb.APIRequests()
	}
	return 0
}

// newFeed builds the 立花 adapter as a read-only feed (公開鍵認証)。
func newFeed() (port.MarketFeed, error) {
	env := os.Getenv("STOCKBOT_TACHIBANA_ENV")
	if env != "demo" && env != "production" {
		return nil, fmt.Errorf("STOCKBOT_TACHIBANA_ENV は demo か production を指定してください (got %q)", env)
	}
	cr, err := broker.TachibanaCredsFromEnv(false)
	if err != nil {
		return nil, err
	}
	// ocoVerified=false / marginEnabled=false: 参照専用。
	return broker.NewTachibana(env, cr.AuthID, cr.Key, cr.SecondPW, false, false, nil), nil
}

// targetSymbols returns the explicit -symbols list, else every <sym>_daily.csv in dir.
func targetSymbols(dir, csv string) ([]string, error) {
	if strings.TrimSpace(csv) != "" {
		var out []string
		for _, s := range strings.Split(csv, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	}
	return candlecsv.DailySymbols(dir)
}

// archive moves an existing CSV into dir (created on demand); a missing file is a
// no-op. 削除ではなく退避 — 出所が混在した履歴も監査対象として残す。
func archive(path, dir string) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(dir, filepath.Base(path)))
}

// loadExisting reads the current CSV; a missing file is not an error (new symbol).
func loadExisting(path, sym string) ([]market.Candle, error) {
	cs, err := candlecsv.Load(path, sym, 24*time.Hour)
	if err != nil {
		// candlecsv.Load は fmt.Errorf(%w) で包むので os.IsNotExist は効かない。
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return cs, nil
}

// mergeDaily is **append-only**: existing bars are never rewritten, only days the
// CSV does not have yet are added. 既存が調整済み・立花が未調整なので、重なり区間を
// 上書きすると履歴が別規約の系列に化け、バックテストの結果が別物になる。
func mergeDaily(existing, fetched []market.Candle) []market.Candle {
	byDay := make(map[string]market.Candle, len(existing)+len(fetched))
	for _, c := range existing {
		byDay[dayKey(c.OpenTime)] = c
	}
	for _, c := range fetched {
		if _, exists := byDay[dayKey(c.OpenTime)]; exists {
			continue
		}
		byDay[dayKey(c.OpenTime)] = c
	}
	out := make([]market.Candle, 0, len(byDay))
	for _, c := range byDay {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenTime.Before(out[j].OpenTime) })
	return out
}

// mergeBenchmark is the merge policy for bench_topix.csv (edge-eval のベンチ超過
// ゲートの入力)。分割は chain-link で吸収し、分割で説明できない水準差だけを拒否する。
//
// 拒否一本にしていた頃は、1306 の 1:10 分割で strict merge が水準不一致
// を出し続け、**ベンチが7週間古いまま誰も気づかなかった**。止まった
// ベンチで検定するのは別系列を継ぐのと同じくらい静かに壊れる。chain-link してよい根拠は
// 実測 — 立花の 1306 と手元の系列は分割前 217 日ぶんが 1 円まで一致している。
func mergeBenchmark(existing, fetched []market.Candle) ([]market.Candle, error) {
	if len(fetched) == 0 {
		return existing, nil // 休場等。空を書いて履歴を消さない
	}
	if d := benchSameDayConflict(existing, fetched); d != "" {
		return nil, fmt.Errorf("重なり日が一致しない(別系列の疑い): %s", d)
	}
	merged := mergeDaily(existing, fetched)
	adjusted, n := market.ChainLinkSplits(merged)
	if n > 0 {
		fmt.Printf("    ↳ ベンチの分割 %d 件を chain-link で吸収(古い側を調整)\n", n)
	}
	if d := market.SplitDiscontinuity(adjusted); d != "" {
		return nil, fmt.Errorf("分割で説明できない水準差(別系列の疑い): %s", d)
	}
	return adjusted, nil
}

// benchSameDayTolerance: 同じ銘柄の同じ日なので本来は一致する。domain の
// SplitLogMoveThreshold(0.35 ≈ 42%)は実暴落を均さないための閾値で、別指数の
// 取り違え(例 1.37倍)がその下をすり抜けてしまう。
const benchSameDayTolerance = 0.01 // 1%

// benchSameDayConflict reports a same-day disagreement that is NOT a split
// (分割比に一致するずれは chain-link が吸収する)。
func benchSameDayConflict(existing, fetched []market.Candle) string {
	byDay := make(map[string]market.Candle, market.SeamOverlapDays)
	for _, c := range market.RecentWindow(existing, market.SeamOverlapDays) {
		byDay[market.JSTDayKey(c.OpenTime)] = c
	}
	for _, f := range fetched {
		s, ok := byDay[market.JSTDayKey(f.OpenTime)]
		if !ok || s.Close <= 0 || f.Close <= 0 {
			continue
		}
		if _, isSplit := market.SplitRatio(s.Close, f.Close); isSplit {
			continue // 分割 → chain-link に任せる
		}
		if math.Abs(f.Close/s.Close-1) > benchSameDayTolerance {
			return fmt.Sprintf("%s: stored %.1f vs fetched %.1f", market.JSTDayKey(f.OpenTime), s.Close, f.Close)
		}
	}
	return ""
}

// mergeSingleSource is the **立花のみ**(単一ソース)運用のマージ。突き合わせる
// 調整済み系列が無いので、分割は拒否せず chain-link で吸収する(拒否すると銘柄の
// 更新が永久に止まる)。単純分割比に一致しない急変は本物の相場として温存する。
//
// 代償: 遡及が約250本(≈1年)しか無く、過去バックテストでのエッジ検証は不可能。
func mergeSingleSource(existing, fetched []market.Candle) ([]market.Candle, error) {
	merged := mergeDaily(existing, fetched)
	adjusted, n := market.ChainLinkSplits(merged)
	if n > 0 {
		fmt.Printf("    ↳ 分割 %d 件を chain-link で吸収(古い側を調整)\n", n)
	}
	// 残る断裂は本物の急変か異常値。単一ソースでは判別材料が無いので、書き込みは
	// 通しつつ人間に報告する。
	if d := market.SplitDiscontinuity(adjusted); d != "" {
		fmt.Printf("    ⚠ 単純分割比に一致しない急変が残っています(実相場か要確認): %s\n", d)
	}
	return adjusted, nil
}

// dayKey delegates to domain/market の JST 正規化(TIMESTAMPTZ が UTC で返る環境でズレない)。
func dayKey(t time.Time) string { return market.JSTDayKey(t) }

// formatCSV renders the existing on-disk format: DateJST;Open;High;Low;Close;Volume
// (semicolon-separated, no header — matches candlecsv.Load).
func formatCSV(cs []market.Candle) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(c.OpenTime.In(clock.JST).Format("2006-01-02"))
		for _, v := range []float64{c.Open, c.High, c.Low, c.Close, c.Volume} {
			b.WriteByte(';')
			b.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// writeCSV writes atomically (tmp + rename) so a crash never leaves a half file.
func writeCSV(path string, cs []market.Candle) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(formatCSV(cs)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
