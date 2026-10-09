// Package pairdiff はペア比較の**本命の統計量**を出す。
//
// 4 つのトレンド系戦略について、**入口が完全に同一で出口だけが違う**
// 兄弟アーム(`X` = 固定 TP / `X_trail` = トレール)を並走させる。問いは
// 「固定 TP と トレール のどちらが良いか」であって、各アーム単独の net ではない。
//
// 🛑 **ペアは独立ではない**。両アームは同じ銘柄・同じ向き・同じ時刻を持ち、
// 損益が強く相関する。だから:
//
//   - **アーム単独の net を 2 群比較してはいけない** — 独立性を仮定した検定になる。
//   - **ポートフォリオ全体の net としては二重計上**になる(同じトリガーを 2 回建てている)。
//   - 正しいのは**対応のある比較**: 同一トリガーの (trail net − capped net) を統計量に取る。
//     相関が高いほど差の分散は小さくなるので、独立2群より**検出力が高い**。
//
// 既存の `forward-report → edge-judge` は戦略別 net の bootstrap で、この量を出せない。
// 本パッケージがその欠けている 1 段。
package pairdiff

import (
	"math"
	"sort"

	"stockbot/backend/internal/domain/strategy"
)

// Trade は突き合わせに要る最小限。query.ForwardTradeView から詰め替える。
type Trade struct {
	Symbol     string
	Strategy   string
	EntryPrice float64
	NetJPY     float64
	Day        string // JST "2006-01-02"(day-block bootstrap のブロックキー)
}

// Pair は成立した 1 組。Diff = trail − capped が統計量。
type Pair struct {
	Symbol string
	Base   string // 基のアーム名(capped 側)
	// EntryPrice は capped 脚の建値(¥1M 正規化の分母もこちら)。
	EntryPrice float64
	// 🚨 **両脚ぶん残す。** 事前登録は「同一銘柄・**同一建値**」と書いているが、
	// 実装は建値 2% 許容の最近傍へ緩めてある。緩めた事実だけ書いて
	// **実際にどれだけずれたかを出さない**と、事後の裁量と区別が付かない。
	// 2% は ¥1M 正規化後で約 ¥20,000/¥1M = 測ろうとしている出口効果と同じ桁。
	CappedEntry float64
	TrailEntry  float64
	// EntryGapPct は |capped − trail| / max(両者)。0 なら事前登録どおりの同一建値。
	EntryGapPct float64
	Day         string
	CappedNet   float64
	TrailNet    float64
	Diff        float64
}

// Unmatched は**片側しか建たなかった**トリガー。survivorship bias の実体で、
// 🚨 **件数を出さないと「ペアが揃ったものだけ」を見て結論を出すことになる**。
// コスト床は trail 側だけ 3 倍厳しい(対象が 1.0×ATR で v2 は 3.0×ATR)ので、
// **壊れたペアは高コスト銘柄で選択的に trail 側から消える**。
type Unmatched struct {
	Symbol     string
	Strategy   string // 実際に建った方のアーム名
	EntryPrice float64
	Day        string
	NetJPY     float64
	MissingArm string // 建たなかった方
}

// OpenLeg は**まだ決済されていない**建玉。片側だけ決済済みのトリガーを
// 「建たなかった」と誤ラベルしないために要る。
type OpenLeg struct {
	Symbol     string
	Strategy   string
	EntryPrice float64
	Day        string
}

// Pending は**兄弟脚がまだ建玉中**のトリガー。Unmatched(建たなかった)とは別物。
//
// 🚨 これを Unmatched に混ぜると ①MeanDiff が打ち切り(censoring)で系統的に偏り
// (決済が早い方だけ拾う)②BrokenByArm がコスト床検出器として読めなくなる。
// MaxHold が 63 / 126 営業日の 2 ペアでは影響が支配的。
type Pending struct {
	Symbol     string
	Strategy   string // 決済済みの方
	EntryPrice float64
	Day        string
	OpenArm    string // まだ建玉中の方
}

// Result は 1 ペア(= 1 基アームとその兄弟)ぶんの突き合わせ結果。
type Result struct {
	Base      string
	Pairs     []Pair
	Unmatched []Unmatched
	// Pending は兄弟脚がまだ建玉中のトリガー。**Unmatched と足さない**。
	Pending     []Pending
	MeanDiffJPY float64
	// day-block bootstrap の CI。ブロックが足りなければ呼び手が判定不能にする。
	DayBlocks int
	// 🚨 突合の残差。**必ず開示する** — 事前登録は「同一建値」なので、
	// 0 でないぶんは事前登録からの逸脱そのもの。Max が許容(2%)に張り付いているなら
	// 「同じトリガー」ではなく別の水準で建った組を数えている疑いになる。
	MaxEntryGapPct  float64
	MeanEntryGapPct float64
}

// entryGapPct は 2 脚の建値の相対差。sameTrigger と同じ分母(大きい側)を使う。
func entryGapPct(a, b float64) float64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	return math.Abs(a-b) / math.Max(a, b)
}

// dayKey は「同じ日の同じ銘柄か」。**建値はキーに入れない。**
//
// 🚨 以前は (銘柄, 建値2桁, 日) の完全一致で突き合わせていたが、コスト床のフラップで
// trail 脚が日中に遅れて建つと 1 呼値ぶん建値がずれ、**ペアが消えるうえ両アームに
// 1 件ずつ「建たなかった」が計上されて** BrokenByArm の比が 1 に寄った。
// 同じ (銘柄, 日) の中で**建値が最も近い者どうし**を組む。
func dayKey(symbol, day string) string { return symbol + "|" + day }

// entryTolerance は「同じトリガーとみなす建値の相対差」。呼値数本ぶんの遅れは許すが、
// 別トリガー(別の水準で建った)は組まない。
const entryTolerance = 0.02 // 2%

func sameTrigger(a, b float64) bool {
	if a <= 0 || b <= 0 {
		return false
	}
	return math.Abs(a-b)/math.Max(a, b) <= entryTolerance
}

// MatchWithOpen は決済済みトレードに加えて**まだ建玉中の脚**を受け取り、
// 「片側しか決済トレードが無い」を **建たなかった(Unmatched)** と
// **兄弟脚がまだ建玉中(Pending)** に分ける。
//
// 入力は**その 2 アームぶんだけ**に絞られている必要はない(他アームは無視する)。
func MatchWithOpen(base string, trades []Trade, open []OpenLeg) Result {
	trail := base + strategy.TrailArmSuffix
	capped := map[string][]Trade{}
	trailed := map[string][]Trade{}
	for _, t := range trades {
		k := dayKey(t.Symbol, t.Day)
		switch t.Strategy {
		case base:
			capped[k] = append(capped[k], t)
		case trail:
			trailed[k] = append(trailed[k], t)
		}
	}
	openBy := map[string][]OpenLeg{}
	for _, o := range open {
		if o.Strategy != base && o.Strategy != trail {
			continue
		}
		k := dayKey(o.Symbol, o.Day)
		openBy[k] = append(openBy[k], o)
	}

	res := Result{Base: base}
	keys := map[string]bool{}
	for k := range capped {
		keys[k] = true
	}
	for k := range trailed {
		keys[k] = true
	}
	for k := range keys {
		cs, ts := capped[k], trailed[k]
		// 建値が近い者どうしから貪欲に組む。同一キーの重複トレードも全部残る
		// (以前は map 代入で 2 本目が黙って消えていた)。
		usedT := make([]bool, len(ts))
		for _, c := range cs {
			best, bestD := -1, math.Inf(1)
			for j, tr := range ts {
				if usedT[j] || !sameTrigger(c.EntryPrice, tr.EntryPrice) {
					continue
				}
				if d := math.Abs(c.EntryPrice - tr.EntryPrice); d < bestD {
					best, bestD = j, d
				}
			}
			if best < 0 {
				res.appendLoneLeg(c, base, trail, openBy[k])
				continue
			}
			usedT[best] = true
			tr := ts[best]
			res.Pairs = append(res.Pairs, Pair{
				Symbol: c.Symbol, Base: base, EntryPrice: c.EntryPrice, Day: c.Day,
				CappedEntry: c.EntryPrice, TrailEntry: tr.EntryPrice,
				EntryGapPct: entryGapPct(c.EntryPrice, tr.EntryPrice),
				CappedNet:   c.NetJPY, TrailNet: tr.NetJPY, Diff: tr.NetJPY - c.NetJPY,
			})
		}
		for j, tr := range ts {
			if usedT[j] {
				continue
			}
			res.appendLoneLeg(tr, trail, base, openBy[k])
		}
	}

	// map 走査は順不同。**出力を決定論にする**(同じ入力なら同じ順・同じ CI)。
	sort.Slice(res.Pairs, func(i, j int) bool { return pairLess(res.Pairs[i], res.Pairs[j]) })
	sort.Slice(res.Unmatched, func(i, j int) bool { return unmatchedLess(res.Unmatched[i], res.Unmatched[j]) })
	sort.Slice(res.Pending, func(i, j int) bool { return pendingLess(res.Pending[i], res.Pending[j]) })

	days := map[string]bool{}
	sum, gapSum := 0.0, 0.0
	for _, p := range res.Pairs {
		sum += p.Diff
		days[p.Day] = true
		gapSum += p.EntryGapPct
		if p.EntryGapPct > res.MaxEntryGapPct {
			res.MaxEntryGapPct = p.EntryGapPct
		}
	}
	if n := len(res.Pairs); n > 0 {
		res.MeanEntryGapPct = gapSum / float64(n)
	}
	if n := len(res.Pairs); n > 0 {
		res.MeanDiffJPY = sum / float64(n)
	}
	res.DayBlocks = len(days)
	return res
}

// appendLoneLeg は相手のいない脚を **Pending(まだ建玉中)** か
// **Unmatched(建たなかった)** に振り分ける。
func (r *Result) appendLoneLeg(t Trade, arm, missing string, open []OpenLeg) {
	for _, o := range open {
		if o.Strategy == missing && sameTrigger(t.EntryPrice, o.EntryPrice) {
			r.Pending = append(r.Pending, Pending{
				Symbol: t.Symbol, Strategy: arm, EntryPrice: t.EntryPrice, Day: t.Day, OpenArm: missing,
			})
			return
		}
	}
	r.Unmatched = append(r.Unmatched, Unmatched{
		Symbol: t.Symbol, Strategy: arm, EntryPrice: t.EntryPrice, Day: t.Day,
		NetJPY: t.NetJPY, MissingArm: missing,
	})
}

func pendingLess(a, b Pending) bool {
	if a.Day != b.Day {
		return a.Day < b.Day
	}
	if a.Symbol != b.Symbol {
		return a.Symbol < b.Symbol
	}
	return a.Strategy < b.Strategy
}

func pairLess(a, b Pair) bool {
	if a.Day != b.Day {
		return a.Day < b.Day
	}
	if a.Symbol != b.Symbol {
		return a.Symbol < b.Symbol
	}
	return a.EntryPrice < b.EntryPrice
}

func unmatchedLess(a, b Unmatched) bool {
	if a.Day != b.Day {
		return a.Day < b.Day
	}
	if a.Symbol != b.Symbol {
		return a.Symbol < b.Symbol
	}
	return a.Strategy < b.Strategy
}

// Diffs / Days は bootstrap へ渡す並行スライス(同順・同長)。
func (r Result) Diffs() []float64 {
	out := make([]float64, len(r.Pairs))
	for i, p := range r.Pairs {
		out[i] = p.Diff
	}
	return out
}

func (r Result) Days() []string {
	out := make([]string, len(r.Pairs))
	for i, p := range r.Pairs {
		out[i] = p.Day
	}
	return out
}

// BrokenByArm は「片側のみ」を**欠けた方のアーム別**に数える。
//
// 🚨 検出器の出力。trail 側が欠けた件数が capped 側より**構造的に多い**なら、
// それはコスト床の非対称(trail の対象は 1.0×ATR = v2 の 3 倍厳しい)が効いている証拠で、
// **ペアが揃ったものだけを見た結論は高コスト銘柄を落とした後の話**になる。
func (r Result) BrokenByArm() map[string]int {
	out := map[string]int{}
	for _, u := range r.Unmatched {
		out[u.MissingArm]++
	}
	return out
}
