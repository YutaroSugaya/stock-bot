package app

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
)

// 完成した分足を cmd/backtest が読むのと同じ CSV 形式で書き出す。
// **立花 API は分足の履歴を返さない**(GetKlines は日足のみ)ので、分足検定に要る
// データはこの記録器で貯めるしかない = デモ/本番フィードで初日から回す必要がある。
type CandleRecorder struct {
	dir string

	mu   sync.Mutex
	last map[string]time.Time // "<symbol>|<suffix>" -> 既にディスク上にある最新 OpenTime
}

const recorderHeader = "DateJST;Open;High;Low;Close;Volume"

var recorderJST = clock.JST

func NewCandleRecorder(dir string) *CandleRecorder {
	return &CandleRecorder{dir: dir, last: make(map[string]time.Time)}
}

// now 時点で**完成した**バーだけを追記する。形成中のバーを書くと後で履歴が
// 書き換わる(同じ足が違う値で2度出る)。
func (r *CandleRecorder) Record(symbol string, iv time.Duration, bars []market.Candle, now time.Time) (int, error) {
	if len(bars) == 0 {
		return 0, nil
	}
	suffix := intervalSuffix(iv)
	key := symbol + "|" + suffix
	path := filepath.Join(r.dir, fmt.Sprintf("%s_%s.csv", symbol, suffix))

	r.mu.Lock()
	defer r.mu.Unlock()

	lastWritten, tracked := r.last[key]
	if !tracked {
		// 再起動時の再開点: ファイル末尾行が watermark。
		ts, err := lastRowTime(path)
		if err != nil {
			return 0, err
		}
		lastWritten = ts
	}

	var fresh []market.Candle
	for _, c := range bars {
		if c.Interval <= 0 || c.OpenTime.Add(c.Interval).After(now) {
			continue // forming (or malformed) bar
		}
		if !c.OpenTime.After(lastWritten) {
			continue // already on disk
		}
		fresh = append(fresh, c)
	}
	if len(fresh) == 0 {
		r.last[key] = lastWritten
		return 0, nil
	}

	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return 0, err
	}
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	if os.IsNotExist(statErr) {
		fmt.Fprintln(w, recorderHeader)
	}
	for _, c := range fresh {
		fmt.Fprintf(w, "%s;%s;%s;%s;%s;%s\n",
			c.OpenTime.In(recorderJST).Format("2006-01-02 15:04:05"),
			ftrim(c.Open), ftrim(c.High), ftrim(c.Low), ftrim(c.Close), ftrim(c.Volume))
		lastWritten = c.OpenTime
	}
	if err := w.Flush(); err != nil {
		return 0, err
	}
	r.last[key] = lastWritten
	return len(fresh), nil
}

func intervalSuffix(iv time.Duration) string {
	switch {
	case iv >= 24*time.Hour:
		return "daily"
	case iv >= time.Hour:
		return fmt.Sprintf("%dh", int(iv.Hours()))
	default:
		return fmt.Sprintf("%dm", int(iv.Minutes()))
	}
}

// ファイルが無い / データ行が無いときは zero time。
func lastRowTime(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	defer f.Close()
	var lastLine string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			lastLine = l
		}
	}
	if err := sc.Err(); err != nil {
		return time.Time{}, err
	}
	field, _, _ := strings.Cut(lastLine, ";")
	if field == "" || field == "DateJST" {
		return time.Time{}, nil
	}
	if ts, err := time.ParseInLocation("2006-01-02 15:04:05", field, recorderJST); err == nil {
		return ts, nil
	}
	if ts, err := time.ParseInLocation("2006-01-02", field, recorderJST); err == nil {
		return ts, nil
	}
	return time.Time{}, fmt.Errorf("candle recorder: unparsable last row %q in %s", lastLine, path)
}

// 100 は "100"、100.5 は "100.5"(seed CSV と同じ表記)。
func ftrim(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
