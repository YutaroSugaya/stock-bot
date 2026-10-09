package query

import (
	"context"
	"sort"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

// 寄り前の銘柄判定(go/no-go)を画面に出す。**表示だけ** — 発注経路は読まない。

type GoNoGoSourceView struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Published string `json:"published,omitempty"`
}

// GoNoGoRowView は 1 銘柄の判定(最後の成功行。成功が無ければ最後の行)。
type GoNoGoRowView struct {
	Status     string             `json:"status"` // ok / error
	Verdict    string             `json:"verdict,omitempty"`
	Category   string             `json:"category,omitempty"`
	Confidence string             `json:"confidence,omitempty"`
	Summary    string             `json:"summary,omitempty"`
	Sources    []GoNoGoSourceView `json:"sources,omitempty"`
	JudgedAt   string             `json:"judged_at"`
	Error      string             `json:"error,omitempty"`
	Tracks     []string           `json:"tracks,omitempty"`
}

// GoNoGoView は今日の判定(銘柄 → 判定)。行に無い銘柄は「未判定」。
type GoNoGoView struct {
	Date    string                   `json:"date"`
	Symbols map[string]GoNoGoRowView `json:"symbols"`
	// MissingArmed は arm 済み(発火済み)なのに成功した判定が無い銘柄(食い違いの監視)。
	MissingArmed []string `json:"missing_armed"`
	Error        string   `json:"error,omitempty"`
}

type ListGoNoGo struct {
	reader port.GoNoGoReader
	clock  clock.Clock
}

func NewListGoNoGo(r port.GoNoGoReader, c clock.Clock) *ListGoNoGo {
	if c == nil {
		c = clock.System()
	}
	return &ListGoNoGo{reader: r, clock: c}
}

// Execute は今日(JST)の判定を返す。armed は画面に出ている arm 済みの銘柄。
func (q *ListGoNoGo) Execute(ctx context.Context, armed []string) GoNoGoView {
	v := GoNoGoView{Date: q.clock().In(clock.JST).Format("2006-01-02"), Symbols: map[string]GoNoGoRowView{},
		MissingArmed: []string{}}
	recs, err := q.reader.ForDate(ctx, v.Date)
	if err != nil {
		v.Error = err.Error()
	}
	for sym, r := range recs {
		row := GoNoGoRowView{Status: r.Status, Verdict: r.Verdict, Category: r.Category, Confidence: r.Confidence,
			Summary: r.SummaryJA, Error: r.Error, Tracks: r.Tracks, JudgedAt: r.JudgedAt.In(clock.JST).Format(time.RFC3339)}
		for _, s := range r.Sources {
			row.Sources = append(row.Sources, GoNoGoSourceView{URL: s.URL, Title: s.Title, Published: s.Published})
		}
		v.Symbols[sym] = row
	}
	seen := map[string]bool{}
	for _, sym := range armed {
		if seen[sym] {
			continue
		}
		seen[sym] = true
		if v.Symbols[sym].Status != port.GoNoGoStatusOK {
			v.MissingArmed = append(v.MissingArmed, sym)
		}
	}
	sort.Strings(v.MissingArmed)
	return v
}
