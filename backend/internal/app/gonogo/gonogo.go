// Package gonogo は寄り前の銘柄判定(go/no-go)の組み立て(cmd/gonogo が使う)。
//
// 🛑 **表示と記録だけ**。bot の発注経路はこの package を import しない(isolation_test)。
// 判定の対象は arm と同じ関数(app.PaperArmCandidates / app.LiveArmCandidates)で出す。
package gonogo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	adapter "stockbot/backend/internal/adapter/gonogo"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/domain/ta"
	"stockbot/backend/internal/port"
)

// ---- 入力(決定論の材料) ----

// packetBars は LLM に渡す直近の日足の本数。packetSMA は乖離と出来高倍率の窓(bnf と同じ 25 日)。
const (
	packetBars = 30
	packetSMA  = 25
)

type Bar struct {
	Date   string  `json:"date"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume float64 `json:"volume"`
}

// Packet は 1 銘柄の判定の入力。JSON のハッシュを記録に残す(同じ入力なら同じハッシュ)。
type Packet struct {
	Date          string   `json:"date"`
	Symbol        string   `json:"symbol"`
	Bars          []Bar    `json:"daily_last_30"`
	Dev25Pct      float64  `json:"deviation_from_sma25_pct"`
	VolumeRatio25 float64  `json:"volume_ratio_vs_avg25"`
	Strategies    []string `json:"strategies"`
	Tracks        []string `json:"tracks"`
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// BuildPacket は日足(前営業日までの確定足)から入力を組み、JSON とその sha256 を返す。
func BuildPacket(date, symbol string, daily []market.Candle, strategies, tracks []string) (Packet, []byte, string) {
	p := Packet{Date: date, Symbol: symbol, Strategies: sortedCopy(strategies), Tracks: sortedCopy(tracks)}
	from := len(daily) - packetBars
	if from < 0 {
		from = 0
	}
	for _, c := range daily[from:] {
		p.Bars = append(p.Bars, Bar{Date: c.OpenTime.In(clock.JST).Format("2006-01-02"),
			Open: c.Open, High: c.High, Low: c.Low, Close: c.Close, Volume: c.Volume})
	}
	if n := len(daily); n >= packetSMA {
		closes := make([]float64, n)
		vol := 0.0
		for i, c := range daily {
			closes[i] = c.Close
		}
		for _, c := range daily[n-packetSMA:] {
			vol += c.Volume
		}
		if sma := ta.SMA(closes, packetSMA); sma > 0 {
			p.Dev25Pct = round2((daily[n-1].Close - sma) / sma * 100)
		}
		if avg := vol / packetSMA; avg > 0 {
			p.VolumeRatio25 = round2(daily[n-1].Volume / avg)
		}
	}
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return p, raw, hex.EncodeToString(sum[:])
}

func symbolOf(packet []byte) string {
	var p struct {
		Symbol string `json:"symbol"`
	}
	_ = json.Unmarshal(packet, &p)
	return p.Symbol
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// ---- 判定の対象 ----

// Target は判定する 1 銘柄と、それを arm しうる戦略・トラック。
type Target struct {
	Symbol     string
	Strategies []string
	Tracks     []string
}

const (
	TrackPaper = "paper"
	TrackLive  = "live"
)

// MergeTargets は paper と live の候補を銘柄で 1 本にまとめる(銘柄順)。
func MergeTargets(paper, live []strategy.Candidate) []Target {
	by := map[string]*Target{}
	add := func(cs []strategy.Candidate, track string) {
		for _, c := range cs {
			t := by[c.Symbol]
			if t == nil {
				t = &Target{Symbol: c.Symbol}
				by[c.Symbol] = t
			}
			t.Strategies = appendUnique(t.Strategies, string(c.Strategy))
			t.Tracks = appendUnique(t.Tracks, track)
		}
	}
	add(paper, TrackPaper)
	add(live, TrackLive)
	out := make([]Target, 0, len(by))
	for _, t := range by {
		sort.Strings(t.Strategies)
		sort.Strings(t.Tracks)
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

func appendUnique(xs []string, x string) []string {
	for _, v := range xs {
		if v == x {
			return xs
		}
	}
	return append(xs, x)
}

// ---- 前提 ----

// Preflight は判定を始めてよいか。満たさなければ何もせず終わる(次の起動経路が拾う)。
type Preflight struct {
	Hours           session.TradingHours
	Now             time.Time
	UniverseModTime time.Time // 今日の today.txt の更新時刻(無ければ zero)
	LatestBarDate   string    // 日足データセットの最新の日付(YYYY-MM-DD)
}

// Check は止める理由を返す(空 = 走ってよい)。needUniverse=false は手動(銘柄指定)。
func (p Preflight) Check(needUniverse bool) string {
	now := p.Now.In(clock.JST)
	if !p.Hours.IsTradingDay(now) {
		return "not_trading_day"
	}
	if p.Hours.MinutesUntilClose(now) <= 0 {
		return "after_close"
	}
	today := now.Format("2006-01-02")
	if needUniverse && (p.UniverseModTime.IsZero() || p.UniverseModTime.In(clock.JST).Format("2006-01-02") != today) {
		return "universe_not_selected_today"
	}
	prev, ok := p.Hours.PrevTradingDay(now, 10)
	if !ok {
		return "calendar_exhausted"
	}
	if p.LatestBarDate < prev.In(clock.JST).Format("2006-01-02") {
		return fmt.Sprintf("daily_stale(最新 %s < 前営業日 %s)", p.LatestBarDate, prev.In(clock.JST).Format("2006-01-02"))
	}
	return ""
}

// ---- 実行 ----

// Judger は 1 銘柄の判定(adapter/gonogo.Judge)。
type Judger interface {
	Judge(ctx context.Context, packet []byte) (port.GoNoGoJudgment, string, error)
}

// Store は判定ファイル(adapter/gonogo.Journal)。
type Store interface {
	Succeeded(date string) (map[string]bool, error)
	Append(r port.GoNoGoRecord) error
	WriteMarkdown(date string) error
}

// 起動経路(記録の Trigger)。
const (
	TriggerLaunchd = "launchd"
	TriggerCatchup = "catchup"
	TriggerManual  = "manual"
	TriggerButton  = "button"
)

// Runner は判定の 1 回の実行。**冪等**: その日に成功した判定がある銘柄は判定し直さない。
type Runner struct {
	Journal     Store
	Judge       Judger
	Clock       clock.Clock
	Concurrency int
	// RunDeadline は 1 回の実行の上限時間(0 = 無制限)。
	RunDeadline time.Duration
	Daily       func(symbol string) []market.Candle
	Logf        func(msg string, kv ...any)
	// Trigger は起動経路(各行に残す)。
	Trigger string
}

// Summary は 1 回の実行の結果(ログと人への報告用)。
type Summary struct {
	Targets, Skipped, Judged, Errors, UsageLimited int
	NoGo, Go, Unknown                              int
	Elapsed                                        time.Duration
}

func (r *Runner) Run(ctx context.Context, date string, targets []Target) (Summary, error) {
	start := r.Clock()
	sum := Summary{Targets: len(targets)}
	done, err := r.Journal.Succeeded(date)
	if err != nil {
		return sum, fmt.Errorf("判定ファイルを読めない: %w", err)
	}
	if r.RunDeadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.RunDeadline)
		defer cancel()
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	var mu sync.Mutex
	var appendErr error
	usageLimited := false
	record := func(rec port.GoNoGoRecord) {
		mu.Lock()
		defer mu.Unlock()
		if err := r.Journal.Append(rec); err != nil && appendErr == nil {
			appendErr = err
		}
		switch {
		case rec.Status == port.GoNoGoStatusOK:
			sum.Judged++
			switch rec.Verdict {
			case port.GoNoGoNoGo:
				sum.NoGo++
			case port.GoNoGoGo:
				sum.Go++
			default:
				sum.Unknown++
			}
		case strings.HasPrefix(rec.Error, adapter.ErrUsageLimit.Error()):
			sum.UsageLimited++
		default:
			sum.Errors++
		}
	}

	sem := make(chan struct{}, maxInt(r.Concurrency, 1))
	var wg sync.WaitGroup
	for _, t := range targets {
		if done[t.Symbol] {
			sum.Skipped++
			continue
		}
		t := t
		_, packet, hash := BuildPacket(date, t.Symbol, r.Daily(t.Symbol), t.Strategies, t.Tracks)
		base := port.GoNoGoRecord{Date: date, Symbol: t.Symbol, PromptVersion: adapter.PromptVersion,
			PacketSHA256: hash, Strategies: t.Strategies, Tracks: t.Tracks, Trigger: r.Trigger}
		sem <- struct{}{}
		mu.Lock()
		halted := usageLimited
		mu.Unlock()
		if halted || ctx.Err() != nil {
			<-sem
			rec := base
			rec.JudgedAt, rec.Status = r.Clock(), port.GoNoGoStatusError
			rec.Error = "run_deadline"
			if halted {
				rec.Error = adapter.ErrUsageLimit.Error()
			}
			record(rec)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			j, model, err := r.Judge.Judge(ctx, packet)
			rec := base
			rec.JudgedAt, rec.Model = r.Clock(), model
			if err != nil {
				rec.Status, rec.Error = port.GoNoGoStatusError, err.Error()
				if errors.Is(err, adapter.ErrUsageLimit) {
					rec.Error = adapter.ErrUsageLimit.Error() + ": " + err.Error()
					mu.Lock()
					usageLimited = true
					mu.Unlock()
					stop()
				}
			} else {
				rec.Status, rec.GoNoGoJudgment = port.GoNoGoStatusOK, j
			}
			record(rec)
			r.logf("gonogo_judged", "symbol", t.Symbol, "status", rec.Status, "verdict", rec.Verdict,
				"category", rec.Category, "err", rec.Error)
		}()
	}
	wg.Wait()
	sum.Elapsed = r.Clock().Sub(start)
	if err := r.Journal.WriteMarkdown(date); err != nil && appendErr == nil {
		appendErr = err
	}
	return sum, appendErr
}

func (r *Runner) logf(msg string, kv ...any) {
	if r.Logf != nil {
		r.Logf(msg, kv...)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// gonogoReadFile はテスト用の薄い読み口。
var gonogoReadFile = os.ReadFile
