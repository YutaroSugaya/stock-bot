package gonogo

import (
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
)

// day2 の前兆(前日の出来高 2 倍・乖離はまだ −12% より上)が立った系列。
func day2Setup(sym string, close float64) []market.Candle {
	cs := make([]market.Candle, 60)
	for i := range cs {
		d := time.Date(2026, 8, 1, 0, 0, 0, 0, clock.JST).AddDate(0, 0, i)
		cs[i] = market.Candle{Symbol: sym, Interval: 24 * time.Hour, OpenTime: d,
			Open: close, High: close * 1.01, Low: close * 0.99, Close: close, Volume: 1000}
	}
	cs[59].Close, cs[59].Volume = close*0.93, 2000
	return cs
}

func liveCfgs() (*config.BotConfig, []*config.StrategyConfig, *config.HardLimits) {
	live := &config.BotConfig{Mode: config.ModeLive}
	live.Risk.AccountMaxOpenPositions = 1
	live.Risk.MaxRiskPerTradeJPY = 100000
	live.Selector.MaxPositionNotionalJPY = 800000
	tmpl := &config.StrategyConfig{ConfigID: "live_day2", Symbol: "*", StrategyName: config.StrategyBNFDay2ReversionTrail,
		Mode: config.ModeLive, HoldingMode: config.HoldingMultiday}
	tmpl.Risk.Quantity = 100
	hl := &config.HardLimits{AllowedSymbols: []string{"1111", "2222", "3333"},
		LiveAllowedStrategies: []string{string(config.StrategyBNFDay2ReversionTrail)}}
	hl.Quantity = config.IntRange{Min: 100, Max: 3000}
	return live, []*config.StrategyConfig{tmpl}, hl
}

func TestCandidates_PaperBNFFamilyAndLive(t *testing.T) {
	u := map[string][]market.Candle{
		"1111": day2Setup("1111", 2000),
		"2222": day2Setup("2222", 3000),
		"3333": day2Setup("3333", 9000), // 1 単元 90 万 > live の上限
	}
	paper := &config.BotConfig{}
	paper.Advisor.Entries = []config.StrategyName{config.StrategyBNFDay2Reversion, config.StrategyPostJumpDrift}
	paper.Advisor.PerStrategyN = 1
	paper.Selector.MaxPositionNotionalJPY = 2000000
	live, tmpls, hl := liveCfgs()
	p, l, err := Candidates(Inputs{Universe: u, Paper: paper, Live: live, LiveTemplates: tmpls, HardLimits: hl})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p {
		if c.Strategy != config.StrategyBNFDay2Reversion && c.Strategy != config.StrategyBNFDay2ReversionTrail {
			t.Fatalf("bnf 家族の外を判定対象にした: %s", c.Strategy)
		}
	}
	// paper は入口あたり per_strategy_n + 余裕(建玉で飛ばされるぶん)まで = 3 銘柄全部 × 2 アーム。
	if len(p) != 6 {
		t.Fatalf("paper の候補 = %d", len(p))
	}
	// live は 1 単元の上限で 3333 を落とし、口座の建玉枠の本数までに切る。
	if len(l) != 1 || l[0].Symbol == "3333" {
		t.Fatalf("live の候補 = %+v", l)
	}
}

// allowlist に無い live テンプレートは判定しない(bot も起動しない)。
func TestCandidates_LiveRequiresAllowlist(t *testing.T) {
	u := map[string][]market.Candle{"1111": day2Setup("1111", 2000)}
	live, tmpls, hl := liveCfgs()
	hl.LiveAllowedStrategies = nil
	_, l, err := Candidates(Inputs{Universe: u, Live: live, LiveTemplates: tmpls, HardLimits: hl})
	if err != nil || len(l) != 0 {
		t.Fatalf("allowlist の外: %+v %v", l, err)
	}
}
