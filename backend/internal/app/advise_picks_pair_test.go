package app

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/strategy"
)

// 🚨 `bnf_reversion` と `bnf_reversion_trail` は入口を共有し
// 出口だけが違うのに、**ペアが 1 本も存在しなかった**。直近10営業日の trigger は
// 118本/11銘柄で完全一致なのに picked は 31 vs 10。原因の 1 つがここ —
// 枠の配布が「1 銘柄は 1 戦略まで」で dedup していた。

func TestSelectAdvisePicksGivesTheSameSymbolToBothArms(t *testing.T) {
	ranked := []strategy.Candidate{
		cand("7203", config.StrategyBNFReversion, 1.2),
		cand("7203", config.StrategyBNFReversionTrail, 1.2),
	}
	got := selectAdvisePicks(ranked, nil, 10, 2, 0, nil)
	if len(got) != 2 {
		t.Fatalf("同一銘柄の 2 アームが両方 pick されること: %+v", got)
	}
	seen := map[config.StrategyName]bool{}
	for _, c := range got {
		if c.Symbol != "7203" {
			t.Fatalf("別銘柄が混ざった: %+v", c)
		}
		seen[c.Strategy] = true
	}
	if !seen[config.StrategyBNFReversion] || !seen[config.StrategyBNFReversionTrail] {
		t.Fatalf("2 アームが揃っていない: %v", seen)
	}
}

// 同一 (銘柄, 戦略) は 1 回だけ(同じ枠を二重に食わない)。
func TestSelectAdvisePicksStillDedupsTheSameSymbolAndStrategy(t *testing.T) {
	ranked := []strategy.Candidate{
		cand("7203", config.StrategyBNFReversion, 1.2),
		cand("7203", config.StrategyBNFReversion, 1.1),
	}
	if got := selectAdvisePicks(ranked, nil, 10, 3, 0, nil); len(got) != 1 {
		t.Fatalf("同一 (銘柄, 戦略) が重複して pick された: %+v", got)
	}
}

// isHeld も (銘柄, 戦略)。片方が建玉中でも**もう片方は arm できる**。
// ここが銘柄キーのままだと、v2 が建った瞬間に同銘柄の v2_trail は永久に arm されない。
func TestSelectAdvisePicksSkipsOnlyTheHeldArm(t *testing.T) {
	ranked := []strategy.Candidate{
		cand("7203", config.StrategyBNFReversion, 1.2),
		cand("7203", config.StrategyBNFReversionTrail, 1.2),
	}
	held := func(sym string, name config.StrategyName) bool {
		return sym == "7203" && name == config.StrategyBNFReversion
	}
	got := selectAdvisePicks(ranked, held, 10, 2, 0, nil)
	if len(got) != 1 || got[0].Strategy != config.StrategyBNFReversionTrail {
		t.Fatalf("建玉中でないアームだけが残ること: %+v", got)
	}
}
