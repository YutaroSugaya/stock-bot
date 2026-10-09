package app

import (
	"log/slog"
	"sort"
	"strings"
	"sync"

	"stockbot/backend/internal/config"
)

// ArmedRankedSymbols はランキングで arm 済み(★・発火済み)の銘柄。include が nil なら全戦略。
// 寄り前の go/no-go が判定すべきだった集合で、判定が無い銘柄の監視に使う(表示だけ)。
func ArmedRankedSymbols(groups []RankingGroup, include func(config.StrategyName) bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range groups {
		for _, r := range g.Rows {
			if !r.Armed || seen[r.Symbol] || (include != nil && !include(r.Strategy)) {
				continue
			}
			seen[r.Symbol] = true
			out = append(out, r.Symbol)
		}
	}
	sort.Strings(out)
	return out
}

// GoNoGoMissing は各トラックの「arm 済みなのに判定が無い銘柄」の最新(画面の「未判定を判定」ボタンが判定する集合)。
type GoNoGoMissing struct {
	mu sync.Mutex
	by map[string][]string
}

func (m *GoNoGoMissing) set(track string, syms []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.by == nil {
		m.by = map[string][]string{}
	}
	m.by[track] = append([]string(nil), syms...)
}

// All は全トラックの和(銘柄順)。
func (m *GoNoGoMissing) All() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, syms := range m.by {
		for _, s := range syms {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// GoNoGoGapLog は「arm 済みなのに判定が無い銘柄」をログに出す(go/no-go の候補と arm の食い違いの監視)。
// 画面は何度も更新されるので、集合が変わったときだけ出す。Missing があればトラックの最新をそこへ置く。
type GoNoGoGapLog struct {
	Logger  *slog.Logger
	Track   string
	Missing *GoNoGoMissing
	mu      sync.Mutex
	last    string
	seen    bool
}

func (g *GoNoGoGapLog) Observe(missing []string) {
	if g.Missing != nil {
		g.Missing.set(g.Track, missing)
	}
	key := strings.Join(missing, ",")
	g.mu.Lock()
	changed := !g.seen || key != g.last
	g.last, g.seen = key, true
	g.mu.Unlock()
	if changed && g.Logger != nil {
		g.Logger.Info("gonogo_missing_for_armed", "track", g.Track, "count", len(missing), "symbols", missing)
	}
}
