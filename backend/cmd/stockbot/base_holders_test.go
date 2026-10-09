package main

import (
	"testing"

	"stockbot/backend/internal/app"
	"stockbot/backend/internal/config"
)

// 🛑 建玉の一意性キーを (銘柄, 戦略) に緩めるのは
// **paper(research / harvest)だけ**。live は 1銘柄1ポジのまま。
//
// 決定論 Selector(live トラックと selector 経路)には ConfigSet の base holder だけを
// 渡す = arm 集合に**型として触れない**。ここが ConfigSet をそのまま渡す形に戻ると、
// 実弾で同一銘柄のエクスポージャが最大で戦略数ぶんになる。
func TestBaseHoldersHidesThePerStrategyArmSet(t *testing.T) {
	cs := app.NewConfigSet(&config.StrategyConfig{
		ConfigID: "seed", Symbol: "7203", StrategyName: config.StrategyNoTrade, Mode: config.ModeLive,
	})
	cs.Arm(&config.StrategyConfig{
		ConfigID: "armed", Symbol: "7203", StrategyName: config.StrategyBNFReversion, Mode: config.ModeLive,
	})

	base := baseHolders(map[string]*app.ConfigSet{"7203": cs})
	h, ok := base["7203"]
	if !ok {
		t.Fatal("銘柄が落ちている")
	}
	// base holder は arm の影響を受けない = Selector は 1 銘柄 1 config しか見ない。
	if got := h.ConfigID(); got != "seed" {
		t.Fatalf("base holder が arm 集合を見ている: config_id=%q, want seed", got)
	}
	// Selector が Set しても arm 集合は動かない(逆向きの分離)。
	h.Set(&config.StrategyConfig{ConfigID: "sel", Symbol: "7203", StrategyName: config.StrategyBNFReversion, Mode: config.ModeLive})
	for _, c := range cs.Active() {
		if c.ConfigID == "sel" {
			t.Fatal("Selector の arm が (銘柄, 戦略) の arm 集合に漏れている")
		}
	}
}
