package gonogo

import (
	"fmt"
	"sort"
	"time"

	"stockbot/backend/internal/backtest/judge"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// 採点(判定は後からまとめて読む)。
//
// (日付, 銘柄)で判定と paper の bnf 家族の建玉(その日に建ったもの)を結合し、go / no_go / unknown /
// 後出し / 未判定 の群で net を比べる。使う判定は建った時刻より前の最後の成功行だけ(¥1M 正規化・day-block bootstrap・EDGE_METHODOLOGY の規律)。
// 🛑 差が出るまで LLM に自動の拒否権は持たせない(持たせるなら CLAUDE.md §4 の変更が要る)。

// GroupUnjudged は判定が無い(または失敗した)建玉の群。
const GroupUnjudged = "unjudged"

// GroupLate は建った後に下した判定しか無い建玉の群(画面のボタンの場中の判定など)。建った時点で
// 無かった情報を見ているので、判定の群には入れない。
const GroupLate = "late"

// ScoredTrade は採点に使う 1 本(決済済み・戦略の出口だけ)。
type ScoredTrade struct {
	Symbol     string
	Strategy   string
	EntryPrice float64
	Quantity   int
	NetJPY     float64
	OpenedAt   time.Time
}

func (t ScoredTrade) day() string { return t.OpenedAt.In(clock.JST).Format("2006-01-02") }

func (t ScoredTrade) per1M() float64 {
	return position.Per1MNotional(t.NetJPY, t.EntryPrice, t.Quantity)
}

// ScoreGroup は 1 群の集計。
type ScoreGroup struct {
	Name         string
	N            int
	DayBlocks    int
	NetJPY       float64
	NetPer1MJPY  float64 // ¥1M 正規化の合計
	MeanPer1MJPY float64 // 1 本あたり
	CILo, CIHi   float64 // 1 本あたりの 95% CI(day-block bootstrap・N=0 は 0)
}

type ScoreReport struct {
	PromptVersion string
	Groups        []ScoreGroup // no_go / unknown / go / unjudged の順
	// NoGoByCategory は no_go 群の内訳(判定のカテゴリごと・名前順・空は "none")。
	// bnf は悪材料で売られた銘柄を買うので「悪材料がある」だけの no_go は母集団のほぼ全部に当たる。
	// 効くとすれば構造的な悪材料(不正・上場廃止・希薄化)と一時的な悪材料の差で、それはここでしか見えない。
	NoGoByCategory []ScoreGroup
}

func inPaperFamily(name string) bool {
	for _, f := range PaperFamily {
		if strategy.EntryArmOf(config.StrategyName(name)) == f {
			return true
		}
	}
	return false
}

// Score は判定と建玉を結合して群ごとに集計する。promptVersion が空なら判定の版は 1 つでなければならない
// (版をまたいだ判定は混ぜない)。
func Score(judgments []port.GoNoGoRecord, trades []ScoredTrade, promptVersion string, resamples int, seed int64) (ScoreReport, error) {
	versions := map[string]bool{}
	for _, r := range judgments {
		if r.Status == port.GoNoGoStatusOK {
			versions[r.PromptVersion] = true
		}
	}
	if promptVersion == "" {
		if len(versions) > 1 {
			return ScoreReport{}, fmt.Errorf("判定の版が混ざっている(%v)— -prompt-version で 1 つ選ぶ", keys(versions))
		}
		for v := range versions {
			promptVersion = v
		}
	}
	// (日付, 銘柄) → 成功した判定(書かれた順)。
	byKey := map[string][]port.GoNoGoRecord{}
	for _, r := range judgments {
		if r.Status == port.GoNoGoStatusOK && r.PromptVersion == promptVersion {
			byKey[r.Date+"|"+r.Symbol] = append(byKey[r.Date+"|"+r.Symbol], r)
		}
	}
	names := []string{port.GoNoGoNoGo, port.GoNoGoUnknown, port.GoNoGoGo, GroupLate, GroupUnjudged}
	groups := map[string]*groupAcc{}
	cats := map[string]*groupAcc{}
	for _, t := range trades {
		if !inPaperFamily(t.Strategy) {
			continue
		}
		g, cat := groupOf(byKey[t.day()+"|"+t.Symbol], t.OpenedAt)
		accumulate(groups, g, t)
		if g == port.GoNoGoNoGo {
			if cat == "" {
				cat = "none"
			}
			accumulate(cats, cat, t)
		}
	}
	rep := ScoreReport{PromptVersion: promptVersion}
	for _, n := range names {
		rep.Groups = append(rep.Groups, groups[n].finish(n, resamples, seed))
	}
	for _, c := range keysOf(cats) {
		rep.NoGoByCategory = append(rep.NoGoByCategory, cats[c].finish(c, resamples, seed))
	}
	return rep, nil
}

// groupAcc は 1 群の集計の途中(¥1M 正規化の値と日付を並べて持つ)。
type groupAcc struct {
	vals []float64
	days []string
	net  float64
}

func accumulate(m map[string]*groupAcc, key string, t ScoredTrade) {
	a := m[key]
	if a == nil {
		a = &groupAcc{}
		m[key] = a
	}
	a.vals = append(a.vals, t.per1M())
	a.days = append(a.days, t.day())
	a.net += t.NetJPY
}

// finish は集計を閉じる(nil = 空の群)。
func (a *groupAcc) finish(name string, resamples int, seed int64) ScoreGroup {
	g := ScoreGroup{Name: name}
	if a == nil {
		return g
	}
	g.N, g.NetJPY = len(a.vals), a.net
	seen := map[string]bool{}
	for i, v := range a.vals {
		g.NetPer1MJPY += v
		seen[a.days[i]] = true
	}
	g.DayBlocks = len(seen)
	if g.N > 0 {
		g.MeanPer1MJPY = g.NetPer1MJPY / float64(g.N)
		g.CILo, g.CIHi, _ = judge.BootstrapMeanCIDayBlock(a.vals, a.days, resamples, 0.95, seed)
	}
	return g
}

func keysOf(m map[string]*groupAcc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// groupOf は建った時刻より前の最後の判定の群と、その判定のカテゴリ。
// 判定が建った後にしか無ければ GroupLate、無ければ GroupUnjudged(カテゴリは空)。
func groupOf(rs []port.GoNoGoRecord, openedAt time.Time) (group, category string) {
	if len(rs) == 0 {
		return GroupUnjudged, ""
	}
	group = GroupLate
	for _, r := range rs {
		if r.JudgedAt.Before(openedAt) {
			group, category = r.Verdict, r.Category
		}
	}
	return group, category
}

// BlockOp は live の銘柄停止の操作の記録 1 行(live_symbol_blocks.log)。
type BlockOp struct {
	At     time.Time
	Symbol string
	Action string // block / release
	Note   string
}

// BlockedTrade は live で止めていた間に paper の bnf 家族で建った 1 本(止めた判断の答え合わせ)。
type BlockedTrade struct {
	BlockedAt time.Time
	Note      string
	Trade     ScoredTrade
}

// BlockedTrades は止めていた間(停止の時刻 ≤ 建てた時刻 < 解除の時刻。解除が無ければ end まで)に
// paper で建った bnf 家族の建玉を返す。
func BlockedTrades(ops []BlockOp, trades []ScoredTrade, end time.Time) []BlockedTrade {
	type interval struct {
		from, to time.Time
		note     string
	}
	sort.SliceStable(ops, func(i, j int) bool { return ops[i].At.Before(ops[j].At) })
	open := map[string]*interval{}
	var spans = map[string][]interval{}
	for _, op := range ops {
		switch op.Action {
		case "block":
			if open[op.Symbol] == nil {
				open[op.Symbol] = &interval{from: op.At, note: op.Note}
			}
		case "release":
			if iv := open[op.Symbol]; iv != nil {
				iv.to = op.At
				spans[op.Symbol] = append(spans[op.Symbol], *iv)
				delete(open, op.Symbol)
			}
		}
	}
	for sym, iv := range open {
		iv.to = end
		spans[sym] = append(spans[sym], *iv)
	}
	var out []BlockedTrade
	for _, t := range trades {
		if !inPaperFamily(t.Strategy) {
			continue
		}
		for _, iv := range spans[t.Symbol] {
			if !t.OpenedAt.Before(iv.from) && t.OpenedAt.Before(iv.to) {
				out = append(out, BlockedTrade{BlockedAt: iv.from, Note: iv.note, Trade: t})
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Trade.OpenedAt.Before(out[j].Trade.OpenedAt) })
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
