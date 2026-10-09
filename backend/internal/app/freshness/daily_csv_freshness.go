package freshness

// daily_csv_freshness.go — 日足 CSV(backend/data)がどこまで進んでいるかの観測。
// **読むだけ**: 立花 API を叩かず、CSV も書かず、ファイル末尾行を見るだけ。
//
// bot は CSV を起動時に seed するだけなので、CSV が止まっても取引は劣化せず
// **誰も気づかない**。launchd の朝ジョブが TCC で毎日 no-op に
// なっていても何日も気づけない。実害は検定ツールが古いデータを読むこと。

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"stockbot/backend/internal/adapter/candlecsv"
	"stockbot/backend/internal/app"
)

// dailyCSVBehindNameCap: 222 銘柄が一斉に遅れたときに全部載せると読めなくなるので、
// 件数は正確に・名前は先頭だけ。
const dailyCSVBehindNameCap = 10

// DailyCSVStatus is the read-only view of how current the daily CSVs are.
type DailyCSVStatus struct {
	Through  string `json:"through"` // 判定基準日。空 = 判定不能
	Newest   string `json:"newest"`
	Oldest   string `json:"oldest"`
	WatchedN int    `json:"watched_n"`
	// BehindN は Through に届いていない銘柄数(ファイルが無い銘柄も含む)。
	BehindN int      `json:"behind_n"`
	Behind  []string `json:"behind,omitempty"`
}

// CheckDailyCSV reads the last row of each symbol's daily CSV and
// reports how many are behind `through`.
//
// through == "" は休場カレンダー失効で判定不能。判定できないのに「遅れている」と
// 鳴らすと本物の遅れと区別がつかなくなるので、そのときは数えない。
//
// inPool(allowed_symbols)で**ディレクトリにあるだけの CSV** を絞る。
// 🚨 上場廃止でプールから外した銘柄の CSV は、
// 人間が retired/ へ移すまで「最新でない」と鳴り続けて本物の遅れを隠した。プールから外した銘柄は
// もう誰も取引しないので数えない。🛑 プールに残っている銘柄は日次ユニバース外でも数える。
// nil = 絞らない(従来どおり)。
func CheckDailyCSV(dir string, symbols []string, through string, inPool func(string) bool) DailyCSVStatus {
	out := DailyCSVStatus{Through: through}
	watched := watchedSeries(dir, symbols, inPool)
	behind := make([]string, 0, len(watched))
	for _, sym := range watched {
		day, err := app.LastRowDayJST(seriesPath(dir, sym))
		if err != nil || day == "" {
			// 読めない / 1行も無い = 「最も遅れている」。ここを黙って飛ばすと、
			// CSV がごっそり欠けていても「全部最新」に見える。
			if through != "" {
				behind = append(behind, sym)
			}
			continue
		}
		if out.Newest == "" || day > out.Newest {
			out.Newest = day
		}
		if out.Oldest == "" || day < out.Oldest {
			out.Oldest = day
		}
		if through != "" && day < through {
			behind = append(behind, sym)
		}
	}
	out.WatchedN = len(watched)
	out.BehindN = len(behind)
	sort.Strings(behind)
	if len(behind) > dailyCSVBehindNameCap {
		behind = behind[:dailyCSVBehindNameCap]
	}
	out.Behind = behind
	return out
}

const benchSeriesName = "bench_topix"

// seriesPath maps a watched series name to its CSV。ベンチだけ命名規約から外れる
// (bench_topix.csv であって bench_topix_daily.csv ではない)。
func seriesPath(dir, name string) string {
	if name == benchSeriesName {
		return filepath.Join(dir, benchSeriesName+".csv")
	}
	return candlecsv.DailyFile(dir, name)
}

// watchedSeries returns every series whose freshness matters: the configured
// universe ∪ whatever daily CSVs sit in the dir ∪ the benchmark.
//
// universe だけを見ているとすり抜ける: 何週間も古い
// bench_topix.csv(edge-eval のベンチ超過ゲートの入力)と、売買停止で universe から
// 外したあと止まったままの銘柄。**検定が読むもの全部**が監視対象。
func watchedSeries(dir string, symbols []string, inPool func(string) bool) []string {
	seen := make(map[string]bool, len(symbols)+2)
	out := make([]string, 0, len(symbols)+2)
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, s := range symbols {
		add(s)
	}
	if syms, err := candlecsv.DailySymbols(dir); err == nil {
		for _, sym := range syms {
			if inPool != nil && !inPool(sym) {
				continue // プールから外した銘柄(上場廃止など)の残骸
			}
			add(sym)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, benchSeriesName+".csv")); err == nil {
		add(benchSeriesName)
	}
	sort.Strings(out)
	return out
}

// DailyCSVWatch caches the latest snapshot: the check touches 222 files, so the
// dashboard's 2 秒ポーリングでは走らせない。
type DailyCSVWatch struct {
	mu   sync.RWMutex
	last DailyCSVStatus
	// InPool は allowed_symbols(nil = 絞らない)。プールから外した銘柄の CSV を数えない。
	InPool func(string) bool
}

func (w *DailyCSVWatch) set(s DailyCSVStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = s
}

func (w *DailyCSVWatch) Get() DailyCSVStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.last
}

// refresh re-runs the check and warns while anything is behind. 警告は毎回出す —
// 静かに1回出して流れるログでは「4日間気づかない」が再発する。
func (w *DailyCSVWatch) Refresh(dir string, symbols []string, through string, logger *slog.Logger) {
	s := CheckDailyCSV(dir, symbols, through, w.InPool)
	w.set(s)
	if s.BehindN == 0 {
		return
	}
	logger.Warn("日足 CSV が最新でない — 検定ツール(edge-eval 等)は古いデータを読みます。"+
		"`make fetch-daily` か朝ジョブ(~/.stockbot/logs/morning.log)を確認してください",
		"dir", dir, "behind", s.BehindN, "of", s.WatchedN,
		"newest", s.Newest, "oldest", s.Oldest, "through", through, "symbols", s.Behind)
}
