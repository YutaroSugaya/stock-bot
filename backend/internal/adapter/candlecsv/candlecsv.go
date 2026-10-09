// Package candlecsv は日足 / 分足の CSV ストア(`<dir>/<sym>_daily.csv`)の読み出し口。
// bot の取り込み・fetch-daily・日次総評・universe-screen・backtest が同じ 1 つを通る
// (バックテスト用パッケージを本番が import しないため。命名と列挙を写すと片方だけずれる)。
package candlecsv

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
)

// dailySuffix は日足ファイルの命名(`<sym>_daily.csv`)。
const dailySuffix = "_daily.csv"

// DailyFile は銘柄の日足ファイルのパス。
func DailyFile(dir, symbol string) string { return filepath.Join(dir, symbol+dailySuffix) }

// LoadDaily は銘柄の日足を読む(Load(DailyFile(dir, sym), sym, 24h))。
func LoadDaily(dir, symbol string) ([]market.Candle, error) {
	return Load(DailyFile(dir, symbol), symbol, 24*time.Hour)
}

// DailySymbols は dir にある日足ファイルの銘柄を昇順で返す(bench_topix.csv 等の別名は含まない)。
func DailySymbols(dir string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*"+dailySuffix))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, strings.TrimSuffix(filepath.Base(p), dailySuffix))
	}
	sort.Strings(out)
	return out, nil
}

// Load parses `Timestamp;Open;High;Low;Close;Volume` (optional header;
// bare date when iv >= 24h). Timestamps are JST at the source, stored as UTC.
func Load(path, symbol string, iv time.Duration) ([]market.Candle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open candle csv %q: %w", path, err)
	}
	defer f.Close()

	jst := clock.JST
	r := csv.NewReader(f)
	r.Comma = ';'
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true

	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read candle csv %q: %w", path, err)
	}

	out := make([]market.Candle, 0, len(rows))
	for i, row := range rows {
		if len(row) < 5 {
			continue
		}
		if i == 0 {
			if _, err := strconv.ParseFloat(strings.TrimSpace(row[1]), 64); err != nil {
				continue
			}
		}
		ot, err := parseJSTTime(strings.TrimSpace(row[0]), iv, jst)
		if err != nil {
			return nil, fmt.Errorf("%q line %d: %w", path, i+1, err)
		}
		o := mustFloat(row[1])
		h := mustFloat(row[2])
		l := mustFloat(row[3])
		c := mustFloat(row[4])
		v := 0.0
		if len(row) >= 6 {
			v, _ = strconv.ParseFloat(strings.TrimSpace(row[5]), 64)
		}
		out = append(out, market.Candle{
			Symbol: symbol, Interval: iv, OpenTime: ot.UTC(),
			Open: o, High: h, Low: l, Close: c, Volume: v,
		})
	}
	return out, nil
}

func parseJSTTime(s string, iv time.Duration, jst *time.Location) (time.Time, error) {
	if iv >= 24*time.Hour {
		if t, err := time.ParseInLocation("2006-01-02", s, jst); err == nil {
			return t, nil
		}
	}
	return time.ParseInLocation("2006-01-02 15:04:05", s, jst)
}

func mustFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}
