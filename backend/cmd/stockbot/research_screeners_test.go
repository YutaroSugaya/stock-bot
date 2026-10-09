package main

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/strategy"
)

// research(advisor 経路と dashboard のランキング)に渡す screener は
// `advisor_v2.entries` で絞った集合。一覧に無い入口は research に渡らない。
// live selector の `strategy.LiveArmScreeners` はこの経路を通らない(触っていない)。
func TestResearchScreenersFollowTheConfiguredEntries(t *testing.T) {
	cfg := &config.BotConfig{}
	cfg.Advisor.Entries = []config.StrategyName{config.StrategyBNFReversion}
	got, err := researchScreeners(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("screeners = %d, want 2(bnf_reversion + _trail)", len(got))
	}
	for _, s := range got {
		if strategy.EntryArmOf(s.Name()) != config.StrategyBNFReversion {
			t.Fatalf("一覧に無い入口 %q が research に渡っている", s.Name())
		}
	}
	// 綴りの誤りは起動を止める(黙って落とすとそのアームが永久に標本ゼロ)。
	cfg.Advisor.Entries = []config.StrategyName{"donchian_breakout_v3"}
	if _, err := researchScreeners(cfg); err == nil {
		t.Fatal("未知の入口名で error を返すこと")
	}
}
