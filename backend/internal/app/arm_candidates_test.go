package app

import (
	"context"
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/strategy"
)

// paper の go/no-go の候補は arm と同じ関数で出す(建玉は見ないので入口ごとの枠を呼び手が広げる)。
func TestPaperArmCandidates_SameOrderAsAdvisorPicks(t *testing.T) {
	u := map[string][]market.Candle{
		"6758": crashSeries(), "7203": crashSeries(), "9984": crashSeries(), "4063": calmSeries(30),
	}
	sc := []strategy.Screener{strategy.BNFReversion{}, strategy.BNFReversionTrail{}}
	got := PaperArmCandidates(u, sc, 0, 2, nil)
	want := selectAdvisePicks(strategy.RankCandidates(u, sc), nil, 1000, 2, 0, nil)
	if len(got) != 4 || len(got) != len(want) {
		t.Fatalf("入口ごと 2 本 × 2 アーム: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Symbol != want[i].Symbol || got[i].Strategy != want[i].Strategy {
			t.Fatalf("arm と順序が違う: %d %+v vs %+v", i, got[i], want[i])
		}
	}
}

// paper の 1 単元の金額上限(advisor の universe と同じ篩)を通す。
func TestPaperArmCandidates_AppliesLotCap(t *testing.T) {
	u := map[string][]market.Candle{"7203": crashSeries()} // 終値 1,700 × 100 = 170,000
	if got := PaperArmCandidates(u, []strategy.Screener{strategy.BNFReversion{}}, 100000, 2, nil); len(got) != 0 {
		t.Fatalf("上限を超える銘柄が候補に入った: %+v", got)
	}
}

// live の候補は selector の Tick と同じ篩(arm config・1 単元の金額・計画損失)で、発火済みだけ。
func TestLiveArmCandidates_MatchesSelectorArming(t *testing.T) {
	sel, holders, _ := newSelectorFixtureMulti(t, 2)
	sel.Tick(context.Background())
	var armed []string
	for _, sym := range []string{"4063", "6758", "7203", "9984"} {
		if holders[sym].Get().StrategyName == config.StrategyBNFReversion {
			armed = append(armed, sym)
		}
	}
	u := sel.loadUniverse(context.Background())
	got := LiveArmCandidates(u, sel.tiers[0], 0, 2)
	if len(got) != 2 || got[0].Symbol != armed[0] && got[0].Symbol != armed[1] {
		t.Fatalf("selector の arm と食い違う: got %+v armed %v", got, armed)
	}
	for _, c := range got {
		if holders[c.Symbol].Get().StrategyName != config.StrategyBNFReversion {
			t.Fatalf("selector が arm していない銘柄を候補にした: %s", c.Symbol)
		}
	}
}

func TestLiveArmCandidates_DropsOverRiskAndUntriggered(t *testing.T) {
	u := map[string][]market.Candle{"7203": crashSeries(), "4063": calmSeries(30)}
	tier := SelectorTier{
		Screeners: []strategy.Screener{strategy.BNFReversion{}},
		ArmCfg: func(sym string) *config.StrategyConfig {
			c := &config.StrategyConfig{Symbol: sym, StrategyName: config.StrategyBNFReversion}
			c.Risk.Quantity = 100
			return c
		},
	}
	if got := LiveArmCandidates(u, tier, 0, 5); len(got) != 1 || got[0].Symbol != "7203" {
		t.Fatalf("発火済みだけ: %+v", got)
	}
	if got := LiveArmCandidates(u, tier, 1, 5); len(got) != 0 {
		t.Fatalf("計画損失の上限を超える候補が残った: %+v", got)
	}
}
