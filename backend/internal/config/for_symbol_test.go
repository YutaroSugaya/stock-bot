package config

import "testing"

// "*" のテンプレートは銘柄ごとに複製し、config_id に銘柄を足す(bot と go/no-go が共有する)。
func TestStrategyConfigForSymbol(t *testing.T) {
	base := &StrategyConfig{ConfigID: "live_x", Symbol: "*", Tuning: map[string]float64{"k": 1}}
	c := base.ForSymbol("7203")
	if c == nil || c.Symbol != "7203" || c.ConfigID != "live_x_7203" {
		t.Fatalf("got %+v", c)
	}
	c.Tuning["k"] = 2
	if base.Tuning["k"] != 1 {
		t.Fatal("複製がテンプレートの map を共有している")
	}
	fixed := &StrategyConfig{Symbol: "6594"}
	if fixed.ForSymbol("6594") != fixed || fixed.ForSymbol("7203") != nil {
		t.Fatal("銘柄固定の config")
	}
	var nilCfg *StrategyConfig
	if nilCfg.ForSymbol("7203") != nil {
		t.Fatal("nil")
	}
}
