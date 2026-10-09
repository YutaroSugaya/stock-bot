// Package journal は日次総評の段1 = 決定論パケットを組み立てる。
//
// cmd/daily-review と bot の引け後ジョブの**両方**から呼ぶ。運用を launchd に足さずに
// 済ませるため(人間の作業は make start / make stop だけ、という運用方針)。
//
// READ-ONLY: DB の SELECT と CSV 読みだけ。発注 API も書込 SQL も型として存在しない。
package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"stockbot/backend/internal/adapter/candlecsv"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/query"
)

// Packet は段2 へ渡す全体。
type Packet struct {
	Date string `json:"date"`
	// GeneratedAt は段1 を作った時刻。**段2 はこれを取得時刻として使う**(LLM は時計を
	// 持たないので、無いとそれらしい時刻が捏造される)。
	GeneratedAt string             `json:"generated_at"`
	Ledger      query.DailyJournal `json:"ledger"`
	Market      MarketSection      `json:"market"`
	Health      HealthSection      `json:"health"`
	// Note は段2 への申し送り。**毎回同じ文言**を機械が入れる(人間が消さない)。
	Note string `json:"note"`
}

type MarketSection struct {
	// 取れなかったものは "取得できず" と明記する。想像で埋めない。
	Bench       *BenchView `json:"bench_topix"`
	UniverseN   int        `json:"universe_n"`
	Advancing   int        `json:"advancing"`
	Declining   int        `json:"declining"`
	Unchanged   int        `json:"unchanged"`
	MedianPct   float64    `json:"median_change_pct"`
	Unavailable string     `json:"unavailable,omitempty"`
}

type BenchView struct {
	Date      string  `json:"date"`
	Close     float64 `json:"close"`
	ChangePct float64 `json:"change_pct"`
}

type HealthSection struct {
	UniverseFile    string `json:"universe_file"`
	UniverseSymbols int    `json:"universe_symbols"`
	UniverseMTime   string `json:"universe_mtime"`
	DataDir         string `json:"data_dir"`
	DataNewestMTime string `json:"data_newest_mtime"`
	Note            string `json:"note,omitempty"`
}

const Note = "これは日記であってエッジの証拠ではない。撤退・継続・ロットの判断には使わない(基準は事前にコミットした基準、判定は cmd/edge-judge)。数字は本 JSON が一次ソースで、文章はここからの引用であり再計算しない。"

// Build は 1 日ぶんのパケットを組む。src は台帳、dataDir/universePath は市況と鮮度。
func Build(ctx context.Context, src port.JournalSource, day time.Time, dataDir, universePath string) (Packet, error) {
	ledger, err := query.NewBuildDailyJournal(src).Execute(ctx, day)
	if err != nil {
		return Packet{}, err
	}
	// 🚨 引け時点の建玉に**その日の終値と含み**を埋める。台帳には無い情報で、
	// その日に記録しないと「N 日目にどこにいたか」が永久に失われる。
	ledger.Open.Positions = EnrichOpenPositions(ledger.Open.Positions, dataDir, ledger.Date)
	return Packet{
		Date:        ledger.Date,
		GeneratedAt: clock.System()().In(clock.JST).Format(time.RFC3339),
		Ledger:      ledger,
		Market:      BuildMarket(dataDir, universePath, ledger.Date),
		Health:      BuildHealth(dataDir, universePath),
		Note:        Note,
	}, nil
}

// WriteJSON は ~/.stockbot/journal/YYYY-MM-DD.json。**上書きしてよい** — 台帳と CSV しか
// 読まないので測り直しても同じ数字になる(日記本文 .md の append-only とは別扱い)。
func WriteJSON(outDir string, p Packet) (string, []byte, error) {
	blob, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", nil, err
	}
	path := filepath.Join(outDir, p.Date+".json")
	return path, blob, os.WriteFile(path, append(blob, '\n'), 0o644)
}

func BuildMarket(dataDir, universePath, date string) MarketSection {
	out := MarketSection{}
	day, err := time.ParseInLocation("2006-01-02", date, clock.JST)
	if err != nil {
		out.Unavailable = "日付を解釈できず"
		return out
	}
	if bars, err := candlecsv.Load(filepath.Join(dataDir, "bench_topix.csv"), "TOPIX", 24*time.Hour); err == nil {
		if b, prev, ok := barOn(bars, day); ok {
			v := BenchView{Date: date, Close: b.Close}
			if prev > 0 {
				v.ChangePct = (b.Close - prev) / prev * 100
			}
			out.Bench = &v
		}
	}
	syms, err := ReadUniverse(universePath)
	if err != nil {
		out.Unavailable = "ユニバースを読めず: " + err.Error()
		return out
	}
	var pcts []float64
	for _, s := range syms {
		bars, err := candlecsv.LoadDaily(dataDir, s)
		if err != nil {
			continue
		}
		b, prev, ok := barOn(bars, day)
		if !ok || prev <= 0 {
			continue
		}
		pcts = append(pcts, (b.Close-prev)/prev*100)
	}
	out.UniverseN = len(pcts)
	if len(pcts) == 0 {
		out.Unavailable = "当日の日足がまだ無い(引け直後は翌朝の fetch-daily 待ち)"
		return out
	}
	for _, p := range pcts {
		switch {
		case p > 0:
			out.Advancing++
		case p < 0:
			out.Declining++
		default:
			out.Unchanged++
		}
	}
	sort.Float64s(pcts)
	out.MedianPct = pcts[len(pcts)/2]
	return out
}

// barOn returns the bar on day and the previous close.
func barOn(bars []market.Candle, day time.Time) (market.Candle, float64, bool) {
	want := day.Format("2006-01-02")
	for i, b := range bars {
		if b.OpenTime.In(clock.JST).Format("2006-01-02") != want {
			continue
		}
		if i == 0 {
			return b, 0, true
		}
		return b, bars[i-1].Close, true
	}
	return market.Candle{}, 0, false
}

func BuildHealth(dataDir, universePath string) HealthSection {
	h := HealthSection{UniverseFile: universePath, DataDir: dataDir}
	if syms, err := ReadUniverse(universePath); err == nil {
		h.UniverseSymbols = len(syms)
	} else {
		h.Note = "ユニバースを読めず: " + err.Error()
	}
	if st, err := os.Stat(universePath); err == nil {
		h.UniverseMTime = st.ModTime().In(clock.JST).Format(time.RFC3339)
	}
	if entries, err := os.ReadDir(dataDir); err == nil {
		var newest time.Time
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), "_daily.csv") {
				continue
			}
			if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		if !newest.IsZero() {
			h.DataNewestMTime = newest.In(clock.JST).Format(time.RFC3339)
		}
	}
	return h
}

func ReadUniverse(path string) ([]string, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(blob), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s は空", path)
	}
	return out, nil
}

// DefaultDir は日記の置き場 ~/.stockbot/journal(repo の外 — TCC で launchd から
// Desktop 配下を触れないため)。
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "journal"
	}
	return filepath.Join(home, ".stockbot", "journal")
}

// RefuseEmptyRebuild は「段1 の測り直しが空になった」を **上書きの手前で**落とす。
//
// 🚨 段2 は「段1 JSON はあるが .md が無い」日を後から拾い、**現在の DSN で測り直して
// JSON を上書き**してから LLM に渡す。上書きの根拠(「台帳と CSV しか読まないので同じ
// 数字になる」)は **DB を期間ごとに分けた瞬間に崩れる**。DSN を新しい DB へ
// 切り替えた後に前の期間の日を backfill すると、**決済が 1 行も無い DB** で組み直して
// 全ゼロで上書きし、LLM が「取引ゼロの日」の総評を書く。`.md` は O_EXCL なので
// **二度と書き直せない**。
//
// 🛑 **この関数は journal パッケージに置く。**
// `cmd/stockbot` の中だけに置くと、**その関数のエラー文が案内する手動コマンド**
// (`cmd/daily-review -date X -stage2`)が素通りするという、対策自体が対策を迂回する
// 形になる。呼び手が 2 つある安全弁は、
// 呼び手の側ではなく共有の側に置く。
func RefuseEmptyRebuild(outDir, date string, rebuilt Packet) error {
	raw, err := os.ReadFile(filepath.Join(outDir, date+".json"))
	if err != nil {
		return nil // 既存が無い = 初回。測り直しではないので通す。
	}
	var prev Packet
	if err := json.Unmarshal(raw, &prev); err != nil {
		return nil // 読めない古い形式。ここで運用を止めない。
	}
	if LedgerRows(prev) > 0 && LedgerRows(rebuilt) == 0 {
		return fmt.Errorf("段1 の測り直しが空になった(%s): 既存 JSON は 決済 %d / 建玉 %d 件を持つのに、"+
			"いまの DSN では 0 件。**DB を取り違えている**(期間の境界で DSN を切り替えた後に "+
			"前の期間の日を backfill しようとしている)。上書きも段2 も行わない — "+
			"旧台帳の DSN を指して `cmd/daily-review -date %s -db harvest -stage2` を回すこと",
			date, prev.Ledger.Trades.N, prev.Ledger.Open.N, date)
	}
	return nil
}

// LedgerRows は「その日の台帳に何かあったか」の粗い指標。決済と建玉だけを見る
// (rejections / screens は DB を取り違えても偶然埋まりうる)。
func LedgerRows(p Packet) int { return p.Ledger.Trades.N + p.Ledger.Open.N }

func DefaultUniversePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "universe/today.txt"
	}
	return filepath.Join(home, ".stockbot", "universe", "today.txt")
}

// UniversePathFor は対象日に応じたユニバースファイル。**今日なら today.txt、
// 過去日なら archive/<date>.txt**。呼び手が毎回この分岐を書き忘れないための 1 箇所。
//
// 🚨 手動 CLI が無条件に today.txt を見ていたせいで、過去日の backfill が
// **その日に見ていた銘柄ではなく今日の銘柄**で市況を組み、段2 の O_EXCL で
// 永久に固定される形になる。
func UniversePathFor(day, now time.Time) string {
	if day.Format("2006-01-02") == now.Format("2006-01-02") {
		return DefaultUniversePath()
	}
	return ArchiveUniversePath(day.Format("2006-01-02"))
}

// ArchiveUniversePath は**過去日**のユニバース。`today.txt` は毎朝上書きされるので、
// 過去日の市況を今日のユニバースで組むと「その日に見ていた銘柄」とズレる
// (`cmd/universe-screen` が同じ内容を日付つきで archive に残している)。
func ArchiveUniversePath(date string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("universe", "archive", date+".txt")
	}
	return filepath.Join(home, ".stockbot", "universe", "archive", date+".txt")
}

// EnrichOpenPositions は引け時点の建玉に **その日の終値と含み**を埋める。
//
// 🚨 なぜ台帳ではなく日記に要るのか:
// 台帳が持つ MFE/MAE は *走っている極値* であって時系列ではない。「その建玉が
// 3 日目にどこにいたか」は**その日に記録しないと永久に失われる**。日次スナップ
// ショットの積み重ねだけがその経路を作る。パケットの .json は残り続けるので、
// ここが実質のアーカイブになる(.md の文章ではなく)。
//
// 🛑 **当日の終値で測る。**その時刻の気配ではなく確定した終値なので、後から
// 測り直しても同じ数字になる(段1 の測り直しが成立する条件)。
// 🛑 **単位を混ぜない。**MFE/MAE が円/株なので含みも円/株を主に置き、
// 口座に効く金額(円/株 × 株数)は別フィールドにする。
func EnrichOpenPositions(open []query.JournalOpenView, dataDir, date string) []query.JournalOpenView {
	day, err := time.ParseInLocation("2006-01-02", date, clock.JST)
	if err != nil {
		return open
	}
	out := make([]query.JournalOpenView, 0, len(open))
	for _, p := range open {
		bars, err := candlecsv.LoadDaily(dataDir, p.Symbol)
		if err != nil {
			p.Unavailable = "日足を読めず"
			out = append(out, p)
			continue
		}
		b, _, ok := barOn(bars, day)
		if !ok || b.Close <= 0 {
			// 🛑 0 で埋めない。引け直後は当日の日足がまだ無いのが普通で、
			// そこを「含みゼロ」と書くと日記が静かに嘘をつく。
			p.Unavailable = "当日の日足がまだ無い(翌朝の fetch-daily 待ち)"
			out = append(out, p)
			continue
		}
		sign := 1.0
		if p.Side == "SELL" {
			sign = -1
		}
		p.ClosePrice = b.Close
		p.UnrealizedPerShareJPY = sign * (b.Close - p.EntryPrice)
		p.UnrealizedJPY = p.UnrealizedPerShareJPY * float64(p.Quantity)
		out = append(out, p)
	}
	return out
}
