package risk

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

// ナンピン禁止のキーは (銘柄, 側, **戦略**)。
// 緩めるのは「別戦略か」の 1 点だけで、**同一戦略の積み増しは従来どおり hard block**。
//
// 🛑 適用は **paper のみ**。`mode: live_config` は 1銘柄1ポジのまま ——
// 同一銘柄のエクスポージャが最大で戦略数ぶん(現メニューで 9 倍)になるので、
// 研究モードの設定を実弾に持ち込まない。

func nanpinSig(name config.StrategyName, side order.Side) strategy.Signal {
	return strategy.Signal{
		Decision: strategy.DecisionEnter, SignalID: "s1", Symbol: "7203",
		Side: side, Quantity: 100, EntryPrice: 2000,
		StrategyName: name, HoldingMode: order.HoldingMultiday,
	}
}

func nanpinCfg(mode config.Mode) *config.StrategyConfig {
	c := &config.StrategyConfig{Symbol: "7203", Mode: mode, HoldingMode: order.HoldingMultiday}
	c.Entry.Direction = config.DirectionBoth
	c.Entry.MaxSpreadTicks = 5
	c.Risk.Quantity = 100
	c.Risk.MaxOpenPositions = 1
	return c
}

// 建玉枠(MaxOpenPositions)ではなくナンピン禁止だけを見たいので、枠は開けておく。
func nanpinSnap() AccountSnapshot {
	return AccountSnapshot{AccountMaxOpenPositions: 400}
}

// 別戦略なら同一銘柄・同一側でも通る(これが兄弟アームのペアを成立させる 1 点)。
func TestNanpinAllowsADifferentStrategyOnTheSameSymbolInPaper(t *testing.T) {
	cfg := nanpinCfg(config.ModePaper)
	cfg.Risk.MaxOpenPositions = 0 // 枠は無制限(研究モード)
	snap := nanpinSnap()
	snap.OpenBuyInclExternal = 1             // bnf_reversion が既に 1 本
	snap.OpenBuySameStrategyInclExternal = 0 // ただし **この戦略では** 0 本

	d := EvaluateStructural(nanpinSig(config.StrategyBNFReversionTrail, order.SideBuy), cfg, snap, nil)
	if !d.Allowed {
		t.Fatalf("別戦略の建玉はナンピンではない: reason=%q", d.Reason)
	}
}

// 同一戦略の積み増しは従来どおり hard block(override 不可)。
func TestNanpinStillBlocksTheSameStrategyInPaper(t *testing.T) {
	cfg := nanpinCfg(config.ModePaper)
	cfg.Risk.MaxOpenPositions = 0
	snap := nanpinSnap()
	snap.OpenBuyInclExternal = 1
	snap.OpenBuySameStrategyInclExternal = 1

	d := EvaluateStructural(nanpinSig(config.StrategyBNFReversion, order.SideBuy), cfg, snap, nil)
	if d.Allowed {
		t.Fatal("同一戦略の積み増しが通った — ナンピン禁止は override 不可の hard gate")
	}
	if d.Reason == "" {
		t.Fatal("reason が空")
	}
}

// external(人間が証券アプリで建てた)建玉は**戦略が分からない**ので、どの戦略に対しても
// 同一側をブロックする。CLAUDE.md の「external 含む」を緩めない。
func TestNanpinBlocksExternalPositionsForEveryStrategy(t *testing.T) {
	cfg := nanpinCfg(config.ModePaper)
	cfg.Risk.MaxOpenPositions = 0
	snap := nanpinSnap()
	// external 1 本のみ。bot の同一戦略の建玉は無い。
	snap.OpenBuyInclExternal = 1
	snap.OpenBuySameStrategyInclExternal = 1 // external は「戦略不明」として必ず数える

	d := EvaluateStructural(nanpinSig(config.StrategyAbsMomentumV2, order.SideBuy), cfg, snap, nil)
	if d.Allowed {
		t.Fatal("external 建玉と同一側が通った — 実弾で二重に持つ")
	}
}

// 🛑 live は 1銘柄1ポジのまま。戦略が違っても同一銘柄・同一側は通さない。
func TestNanpinKeepsTheSymbolKeyForLive(t *testing.T) {
	cfg := nanpinCfg(config.ModeLive)
	cfg.Risk.MaxOpenPositions = 0
	snap := nanpinSnap()
	snap.OpenBuyInclExternal = 1
	snap.OpenBuySameStrategyInclExternal = 0 // 別戦略の建玉

	d := EvaluateStructural(nanpinSig(config.StrategyBNFReversionTrail, order.SideBuy), cfg, snap, nil)
	if d.Allowed {
		t.Fatal("live で別戦略の同一銘柄が通った — 研究モードの緩和が実弾へ漏れている")
	}
}

// 売り側も同じ(SELL もメニューに入る)。
func TestNanpinPerStrategyAppliesToTheSellSideToo(t *testing.T) {
	cfg := nanpinCfg(config.ModePaper)
	cfg.Risk.MaxOpenPositions = 0
	snap := nanpinSnap()
	snap.OpenSellInclExternal = 1
	snap.OpenSellSameStrategyInclExternal = 0

	if d := EvaluateStructural(nanpinSig(config.StrategyBNFReversionTrail, order.SideSell), cfg, snap, nil); !d.Allowed {
		t.Fatalf("売り: 別戦略なら通ること: %q", d.Reason)
	}
	snap.OpenSellSameStrategyInclExternal = 1
	if d := EvaluateStructural(nanpinSig(config.StrategyBNFReversion, order.SideSell), cfg, snap, nil); d.Allowed {
		t.Fatal("売り: 同一戦略の積み増しが通った")
	}
}
