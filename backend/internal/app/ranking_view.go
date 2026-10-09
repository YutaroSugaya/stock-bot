package app

import (
	"context"
	"sort"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// rankingPerStrategyN は 1 戦略あたりの表示行数。arm 済みはこの枠外でも必ず出す。
const rankingPerStrategyN = 3

// RankingRow is the dashboard view of one scan candidate.
type RankingRow struct {
	strategy.Candidate
	// Armed = arm 済みでまだ建玉していない(= 発注待ち)。★ はこれ。
	Armed bool `json:"armed"`
	Held  bool `json:"held"`
	// BlockedNotional = 発火はしたが 1単元が建玉金額上限を超えていて **arm されない**。
	// 資金上限はスクリーナーの外側にあるので、印を付けないと発注直前の候補に見える。
	BlockedNotional bool `json:"blocked_notional"`
}

type RankingGroup struct {
	Strategy  string `json:"strategy"`
	Triggered int    `json:"triggered"`
	// 🛑 Triggered の**向き別内訳**。`direction: both` の 4 戦略は
	// 売りも出すのに、戦略名だけで畳んでいたため買いと売りが 1 つの数字に合算され、
	// **売りが何件出たかを画面から知る方法が無かった**(実測: donchian は
	// 買い10 + 売り3 = 13 と表示)。事前登録は「売りの標本ゼロ」を
	// 一覧が空 / 真に非貸借 / そもそも発火していない に切り分けると事前登録している。
	// Buy + Sell == Triggered(Side 未設定の買い専用スクリーナーは Buy に数える)。
	Buy   int `json:"buy"`
	Sell  int `json:"sell"`
	Armed int `json:"armed"`
	// Triggered のうち資金上限で arm 対象外の件数。**Triggered からは引かない** —
	// 黙って減らすと「トリガーが減った」と「対象が最初から狭い」が区別できなくなる。
	BlockedNotional int          `json:"blocked_notional"`
	Rows            []RankingRow `json:"rows"`
}

// groupRanking builds the dashboard ranking **grouped by strategy**.
//
// フラットな「全体 score 降順の上位N」は使えない: スコアの定義が戦略ごとに違って
// 比較できず(abs_momentum は 現在値/200日線 で上限なし、donchian は構造上 1.0 付近が
// 上限)、abs_momentum が表示枠を独占する。実測: arm 済み 9 銘柄のうち
// 7 銘柄が上位15圏外で不可視 = 画面から bot が何を待っているのか読めなかった。
//
// armed / held は **(銘柄, 戦略) キー**(`armKey`)の集合。1 銘柄は複数のスクリーナーで
// 同時にトリガーするので、「arm 済みか」を銘柄だけで見ると同じ銘柄に何度も ★ が付き、
// 実行されない戦略が発注を待っているように見える。★ は **実行される戦略のグループに
// だけ** 付ける。1 銘柄に複数戦略が同時に載るようになったので、held も銘柄だけで
// 見ると片方の建玉がもう片方のアームを「建玉あり」に見せる。
// 🛑 戦略不明の建玉(external / 旧建玉)は `armKey(sym, "")` に入り、**その銘柄の全戦略**を
// 建玉ありとして扱う — ナンピン禁止ゲートが同じ倒し方をするので、画面と実際の挙動を揃える。
// unaffordable は「1単元が建玉金額上限を超える候補」を **(銘柄, 戦略) キー**
// (`UnaffordableKey`)で持つ(nil = 未計測 → 印を付けない)。
func groupRanking(ranked []strategy.Candidate, armed map[string]bool, held map[string]bool, topN int,
	unaffordable map[string]bool) []RankingGroup {
	if topN <= 0 {
		topN = 1
	}
	byStrategy := map[config.StrategyName][]strategy.Candidate{}
	for _, c := range ranked {
		if !c.Triggered {
			continue
		}
		byStrategy[c.Strategy] = append(byStrategy[c.Strategy], c)
	}
	groups := make([]RankingGroup, 0, len(byStrategy))
	for st, cands := range byStrategy {
		g := RankingGroup{Strategy: string(st), Triggered: len(cands)}
		for i, c := range cands {
			if c.Side == order.SideSell {
				g.Sell++
			} else {
				g.Buy++ // Side 未設定 = 買い専用スクリーナー
			}
			isHeld := held[armKey(c.Symbol, st)] || held[armKey(c.Symbol, "")]
			// arm された戦略と一致する行にだけ ★(重複カウント防止)。
			isArmed := armed[armKey(c.Symbol, st)] && !isHeld
			if isArmed {
				g.Armed++
			}
			// 建玉中は上限の話ではない(既に建っている)ので印を付けない。
			// 🛑 キーは **(銘柄, 戦略)** — 優先ティアごとに建玉金額上限が違う
			// (live: bnf 550,000 / donchian_v2_trail 300,000)ので、銘柄だけで引くと
			// 「donchian の上限で落ちた」ことが bnf の行にも印を付ける。
			blocked := unaffordable[UnaffordableKey(c.Symbol, st)] && !isHeld
			if blocked {
				g.BlockedNotional++
			}
			// topN 圏外でも arm 済みは必ず出す(発注待ちが見えないのが元の不具合)。
			if i < topN || isArmed {
				g.Rows = append(g.Rows, RankingRow{Candidate: c, Armed: isArmed, Held: isHeld, BlockedNotional: blocked})
			}
		}
		groups = append(groups, g)
	}
	// トリガー数降順(同数は戦略名昇順で安定させる) = 今日どの戦略が効く相場かを上から読む。
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Triggered != groups[j].Triggered {
			return groups[i].Triggered > groups[j].Triggered
		}
		return groups[i].Strategy < groups[j].Strategy
	})
	return groups
}

func RankingView(ctx context.Context, sp *ScanProvider, bundles []*SymbolBundle,
	posRepo port.PositionRepository, sel *Selector) []RankingGroup {
	held := heldSymbols(ctx, posRepo)
	// 1 銘柄に複数戦略が載るようになったので、arm 済み戦略は**全部**出す
	// (1 本目だけを出すと、ペアの片方が画面から消えて「片側しか arm されていない」
	// ように見える)。
	armed := make(map[string]bool, len(bundles))
	for _, b := range bundles {
		if b.Configs == nil {
			continue
		}
		for _, c := range b.Configs.Active() {
			if c != nil && c.StrategyName != config.StrategyNoTrade {
				armed[armKey(b.Symbol, c.StrategyName)] = true
			}
		}
	}
	// 資金上限の判定は Selector が持つ(発火判定は資金を見ない)。selector 無し = 未計測。
	var tooBig map[string]bool
	if sel != nil {
		tooBig = sel.UnaffordableSymbols()
	}
	return groupRanking(sp.Ranking(ctx), armed, held, rankingPerStrategyN, tooBig)
}

// heldSymbols returns the symbols with an OPEN/CLOSING position. A repo error
// yields an empty set: the ★ is display-only, so a failed read shows no ★ rather
// than painting held symbols as "waiting to order".
// armKey は (銘柄, 戦略) キー。銘柄だけで arm / held を判定すると、
// 入口が同一で出口だけ違う 2 アームが画面上で区別できない。
func armKey(symbol string, name config.StrategyName) string { return symbol + "|" + string(name) }

// heldSymbols は建玉を **(銘柄, 戦略) キー**で返す。戦略不明(external / 旧建玉)は
// `armKey(sym, "")` に入れる — 呼び手はそれを「その銘柄の全戦略が建玉あり」と読む。
func heldSymbols(ctx context.Context, posRepo port.PositionRepository) map[string]bool {
	out := map[string]bool{}
	if posRepo == nil {
		return out
	}
	open, err := posRepo.ListOpenAllSymbols(ctx)
	if err != nil {
		return out
	}
	for _, p := range open {
		out[armKey(p.Symbol, config.StrategyName(p.StrategyName))] = true
	}
	return out
}
