package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

func day(y int, m time.Month, d int, c float64) market.Candle {
	return market.Candle{OpenTime: time.Date(y, m, d, 0, 0, 0, 0, clock.JST), Open: c, High: c, Low: c, Close: c, Volume: 1}
}

// 立花は 250本程度しか返さないので、上書きすると長い既存履歴が消える。
func TestMergeKeepsHistoryAndAppendsNew(t *testing.T) {
	existing := []market.Candle{day(2026, 6, 20, 100), day(2026, 6, 21, 101)}
	fetched := []market.Candle{day(2026, 6, 21, 999), day(2026, 6, 22, 102)}

	out := mergeDaily(existing, fetched)

	if len(out) != 3 {
		t.Fatalf("len = %d, want 3 (6/20, 6/21, 6/22)", len(out))
	}
	if !out[0].OpenTime.Before(out[1].OpenTime) || !out[1].OpenTime.Before(out[2].OpenTime) {
		t.Fatalf("not sorted ascending: %+v", out)
	}
	if out[1].Close != 101 {
		t.Fatalf("append-only: 既存の同日バーは保持されるべき(調整済み履歴を壊さない): got %v", out[1].Close)
	}
	if out[0].Close != 100 {
		t.Fatal("既存の古いバーが消えている(履歴破壊)")
	}
}

func TestMergeEmptyFetchKeepsExisting(t *testing.T) {
	existing := []market.Candle{day(2026, 6, 20, 100)}
	out := mergeDaily(existing, nil)
	if len(out) != 1 || out[0].Close != 100 {
		t.Fatalf("既存が保持されていない: %+v", out)
	}
}

// 同一日が複数返っても1本に畳む。append-only なので先に見たバーが残る。
func TestMergeDedupesSameDay(t *testing.T) {
	fetched := []market.Candle{day(2026, 6, 22, 1), day(2026, 6, 22, 2)}
	out := mergeDaily(nil, fetched)
	if len(out) != 1 {
		t.Fatalf("dedupe されていない: %+v", out)
	}
	if out[0].Close != 1 {
		t.Fatalf("append-only では最初のバーが残るはず: %v", out[0].Close)
	}
}

func TestFormatCSV(t *testing.T) {
	got := formatCSV([]market.Candle{{
		OpenTime: time.Date(2026, 6, 22, 15, 0, 0, 0, clock.JST),
		Open:     2921.5, High: 2951, Low: 2897.5, Close: 2939, Volume: 24733500,
	}})
	want := "2026-06-22;2921.5;2951;2897.5;2939;24733500\n"
	if got != want {
		t.Fatalf("csv =\n%q\nwant\n%q", got, want)
	}
}

// 重なり区間を上書きすると調整済み履歴が静かに別物になる(1年分を
// 上書きして BNF 再現バックテストが N=2786/PF1.523 → 2698/1.266 に壊れた)。
func TestMergeIsAppendOnly(t *testing.T) {
	existing := []market.Candle{day(2026, 6, 19, 100), day(2026, 6, 22, 101)}
	fetched := []market.Candle{day(2026, 6, 22, 103), day(2026, 6, 23, 104)}

	out := mergeDaily(existing, fetched)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	if out[1].Close != 101 {
		t.Fatalf("既存バーが上書きされた: %v (want 101 のまま)", out[1].Close)
	}
	if out[2].Close != 104 {
		t.Fatalf("新しいバーが追加されていない: %+v", out[2])
	}
}

// candlecsv.Load は %w で包むので os.IsNotExist では判定できない。
func TestLoadExistingMissingFile(t *testing.T) {
	cs, err := loadExisting(filepath.Join(t.TempDir(), "9999_daily.csv"), "9999")
	if err != nil {
		t.Fatalf("ファイル無しはエラーにしない: %v", err)
	}
	if len(cs) != 0 {
		t.Fatalf("cs = %+v, want empty", cs)
	}
}

func TestWriteCSVAtomicAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "7203_daily.csv")
	bars := []market.Candle{day(2026, 6, 22, 2939)}
	if err := writeCSV(path, bars); err != nil {
		t.Fatalf("writeCSV: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("一時ファイルが残っている")
	}
	back, err := loadExisting(path, "7203")
	if err != nil || len(back) != 1 || back[0].Close != 2939 {
		t.Fatalf("往復しない: %v %+v", err, back)
	}
}

// 単一ソースでは拒否すると銘柄の更新が永久に止まるので、分割は chain-link で吸収する。
func TestMergeChainLinksSplitInSingleSourceMode(t *testing.T) {
	existing := []market.Candle{day(2026, 3, 26, 15000), day(2026, 3, 27, 15195)}
	fetched := []market.Candle{day(2026, 3, 30, 2917)} // 1:5 分割(未調整)

	out, err := mergeSingleSource(existing, fetched)
	if err != nil {
		t.Fatalf("mergeSingleSource: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	if d := market.SplitDiscontinuity(out); d != "" {
		t.Fatalf("chain-link 後も断裂: %s", d)
	}
	if out[1].Close > 4000 { // 15195 / 5 ≈ 3039
		t.Fatalf("古い側が調整されていない: %v", out[1].Close)
	}
}

// 立花の当日足は夜間バッチまで未確定の可能性がある(18:21 の取得でも当日
// バーは返らなかった)。append-only なので形成中バーを一度取り込むと自己修復されない。
func TestFilterFetchedDropsTodayBar(t *testing.T) {
	now := time.Date(2026, 7, 23, 14, 30, 0, 0, clock.JST)
	kept, dropped := filterFetched([]market.Candle{day(2026, 7, 22, 100), day(2026, 7, 23, 99)}, now, false)
	if len(kept) != 1 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d, want 1/1(当日足が固定されると部分足が恒久残留する)", len(kept), dropped)
	}
	if dayKey(kept[0].OpenTime) != "2026-07-22" {
		t.Fatalf("前日の確定バーが残るべき: %s", dayKey(kept[0].OpenTime))
	}
}

func TestFilterFetchedIncludeTodayOverride(t *testing.T) {
	now := time.Date(2026, 7, 23, 18, 0, 0, 0, clock.JST)
	kept, dropped := filterFetched([]market.Candle{day(2026, 7, 23, 99)}, now, true)
	if len(kept) != 1 || dropped != 0 {
		t.Fatalf("include-today で当日足が捨てられた: kept=%d dropped=%d", len(kept), dropped)
	}
}

func TestFilterFetchedMorningRunKeepsYesterday(t *testing.T) {
	now := time.Date(2026, 7, 24, 8, 30, 0, 0, clock.JST)
	kept, dropped := filterFetched([]market.Candle{day(2026, 7, 23, 99)}, now, false)
	if len(kept) != 1 || dropped != 0 {
		t.Fatalf("翌朝の実行で前日確定バーが捨てられた: kept=%d dropped=%d", len(kept), dropped)
	}
}

// fakeFeed is a canned port.MarketFeed for updateFile tests (read-only surface).
type fakeFeed struct {
	klines []market.Candle
	err    error
}

func (f *fakeFeed) GetTicker(context.Context, string) (*market.Ticker, error) { return nil, nil }
func (f *fakeFeed) GetKlines(context.Context, string, port.KlinePeriod, int) ([]market.Candle, error) {
	return f.klines, f.err
}
func (f *fakeFeed) RefreshToken(context.Context) error { return nil }

func TestUpdateFileSkipsFormingTodayBar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "7203_daily.csv")
	if err := writeCSV(path, []market.Candle{day(2026, 7, 21, 100)}); err != nil {
		t.Fatal(err)
	}
	feed := &fakeFeed{klines: []market.Candle{day(2026, 7, 22, 101), day(2026, 7, 23, 99)}}
	o := fetchOpts{bars: 250, now: func() time.Time { return time.Date(2026, 7, 23, 14, 30, 0, 0, clock.JST) }}

	if got := updateFile(context.Background(), feed, "7203", path, o); got != outcomeUpdated {
		t.Fatalf("outcome = %v, want updated", got)
	}
	back, err := loadExisting(path, "7203")
	if err != nil {
		t.Fatal(err)
	}
	if last := dayKey(back[len(back)-1].OpenTime); last != "2026-07-22" {
		t.Fatalf("当日(未確定)バーが書き込まれた: last=%s", last)
	}
}

func TestUpdateFileOnlyTodayBarKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "7203_daily.csv")
	if err := writeCSV(path, []market.Candle{day(2026, 7, 22, 100)}); err != nil {
		t.Fatal(err)
	}
	feed := &fakeFeed{klines: []market.Candle{day(2026, 7, 23, 99)}}
	o := fetchOpts{bars: 250, now: func() time.Time { return time.Date(2026, 7, 23, 12, 0, 0, 0, clock.JST) }}

	if got := updateFile(context.Background(), feed, "7203", path, o); got != outcomeSkipped {
		t.Fatalf("outcome = %v, want skipped", got)
	}
	back, _ := loadExisting(path, "7203")
	if len(back) != 1 || back[0].Close != 100 {
		t.Fatalf("既存 CSV が変更された: %+v", back)
	}
}

// 単純分割比に一致しない急変は均さない — 均すと BNF 逆張りが狙う暴落自体が消える。
func TestMergeSingleSourceKeepsRealCrash(t *testing.T) {
	existing := []market.Candle{day(2026, 3, 26, 3120)}
	fetched := []market.Candle{day(2026, 3, 27, 2420)} // -22.4%(実相場)
	out, err := mergeSingleSource(existing, fetched)
	if err != nil {
		t.Fatalf("mergeSingleSource: %v", err)
	}
	if out[0].Close != 3120 || out[1].Close != 2420 {
		t.Fatalf("実相場の急変が改変された: %+v", out)
	}
}
