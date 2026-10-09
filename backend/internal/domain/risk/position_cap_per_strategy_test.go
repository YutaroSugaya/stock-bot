package risk

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
)

// 🚨 **ペア比較の目的そのものが達成されない欠陥**。
//
// 建玉の一意性キーを (銘柄, 戦略) にしたのに、**建玉枠(MaxOpenPositions)だけが
// 銘柄キーのまま**だった。決定論テンプレートは `max_open_positions: 1` を
// 全アームに凍結するので、同一銘柄の 2 本目は**戦略が違っても必ず落ちる**:
//
//	REJECT reason="open_positions" detail="1 >= cap 1"
//
// = bnf ペアが 0 本になる壊れ方をそのまま再現する。ペア標本も
// 想定した paper 両建ても、構造的に発生しない。
//
// テンプレートの表は **建玉キーが「銘柄」だった時点**のもので、そこでは
// `max_open_positions: 1` = 「同一銘柄は 1 本」で正しかった。キーが変わった以上、
// **枠のキーも同じ粒度に揃える**のが筋(値を 0 にして枠を無効化するのではない —
// それだと同一戦略が同一銘柄で買いと売りを同時に持ててしまう)。

func capSnap() AccountSnapshot { return AccountSnapshot{AccountMaxOpenPositions: 400} }

// 別戦略の建玉は枠を消費しない(paper)。ここがペアが成立する条件。
func TestPositionCapCountsOnlyTheSameStrategyInPaper(t *testing.T) {
	cfg := nanpinCfg(config.ModePaper) // MaxOpenPositions = 1
	snap := capSnap()
	snap.OpenPositions = 1             // 同一銘柄に別戦略の建玉が 1 本
	snap.OpenPositionsSameStrategy = 0 // ただし **この戦略では** 0 本

	d := EvaluateStructural(nanpinSig(config.StrategyBNFReversionTrail, order.SideBuy), cfg, snap, nil)
	if !d.Allowed {
		t.Fatalf("別戦略の建玉が枠を消費している: %q — ペアが 1 本も成立しない", d.Reason)
	}
}

// 同一戦略の 2 本目は従来どおり枠で落ちる(積み増しの禁止は緩めない)。
func TestPositionCapStillBlocksTheSameStrategyInPaper(t *testing.T) {
	cfg := nanpinCfg(config.ModePaper)
	snap := capSnap()
	snap.OpenPositions = 1
	snap.OpenPositionsSameStrategy = 1

	d := EvaluateStructural(nanpinSig(config.StrategyBNFReversion, order.SideBuy), cfg, snap, nil)
	if d.Allowed {
		t.Fatal("同一戦略の 2 本目が枠を素通りした")
	}
}

// 🛑 同一戦略が同一銘柄で**買いと売りを同時に持つ**のは測定として無意味。
// ナンピン禁止は (銘柄, 側, 戦略) キーなので側が違えば通してしまう —
// そこを止めているのが**戦略単位の建玉枠**(側を見ない)である。
func TestPositionCapBlocksTheSameStrategyHedgingItself(t *testing.T) {
	cfg := nanpinCfg(config.ModePaper)
	snap := capSnap()
	snap.OpenPositions = 1
	snap.OpenPositionsSameStrategy = 1 // 買いを 1 本持っている
	snap.OpenSellSameStrategyInclExternal = 0

	d := EvaluateStructural(nanpinSig(config.StrategyBNFReversion, order.SideSell), cfg, snap, nil)
	if d.Allowed {
		t.Fatal("同一戦略が同一銘柄で両建てできてしまう(枠が側を見ずに止めるべき)")
	}
}

// 🛑 live は**銘柄キーのまま**。研究モードの緩和を実弾へ持ち込まない。
func TestPositionCapKeepsTheSymbolKeyForLive(t *testing.T) {
	cfg := nanpinCfg(config.ModeLive)
	snap := capSnap()
	snap.OpenPositions = 1
	snap.OpenPositionsSameStrategy = 0 // 別戦略の建玉

	d := EvaluateStructural(nanpinSig(config.StrategyBNFReversionTrail, order.SideBuy), cfg, snap, nil)
	if d.Allowed {
		t.Fatal("live で別戦略の 2 本目が通った — 研究モードの緩和が実弾へ漏れている")
	}
}
