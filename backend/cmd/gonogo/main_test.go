package main

import (
	"os"
	"path/filepath"
	"testing"

	"stockbot/backend/internal/config"
)

func TestSplitSymbols(t *testing.T) {
	got := splitSymbols(" 6594, 7203,,")
	if len(got) != 2 || got[0] != "6594" || got[1] != "7203" {
		t.Fatalf("%v", got)
	}
}

// ユニバースは bot と同じ読み方で、allowed_symbols の外を落とす。
func TestReadUniverseDropsOutsideWhitelist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "today.txt")
	if err := os.WriteFile(p, []byte("6594\n9999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readUniverse(p, &config.HardLimits{AllowedSymbols: []string{"6594"}})
	if err != nil || len(got) != 1 || got[0] != "6594" {
		t.Fatalf("%v %v", got, err)
	}
}

// 設定は catchup が写した一覧(gonogo-configs.txt)から読む(repo のパスは launchd から読めない)。
func TestLoadInputsReadsTheCopiedManifest(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("bot_config.advisor.yaml", "mode: paper_config\nadvisor_v2:\n  per_strategy_n: 2\n")
	write("bot_config.live.yaml", "mode: live_config\nbroker: { kind: tachibana }\nrisk: { account_max_open_positions: 10 }\n")
	write("strategy_config.live.yaml", "config_id: x\nsymbol: \"*\"\nstrategy_name: bnf_day2_reversion_trail\nmode: live_config\nholding_mode: multiday\nentry: { direction: buy_only }\nrisk: { quantity: 100 }\n")
	write(configManifest, "paper=bot_config.advisor.yaml\nlive_bot=bot_config.live.yaml\nlive_strategy=strategy_config.live.yaml\n")
	in, err := loadInputs(dir, &config.HardLimits{}, nil)
	if err != nil {
		t.Fatalf("loadInputs: %v", err)
	}
	if in.Paper == nil || in.Paper.Advisor.PerStrategyN != 2 || in.Live == nil || len(in.LiveTemplates) != 1 ||
		in.LiveTemplates[0].StrategyName != config.StrategyBNFDay2ReversionTrail {
		t.Fatalf("写しを読めていない: %+v", in)
	}
	// live が無効なら live の行は無い = live は判定しない。
	write(configManifest, "paper=bot_config.advisor.yaml\n")
	if in, err := loadInputs(dir, &config.HardLimits{}, nil); err != nil || in.Live != nil {
		t.Fatalf("live 無効: %+v %v", in.Live, err)
	}
	// 一覧が無ければ失敗(catchup がまだ写していない)。
	if _, err := loadInputs(t.TempDir(), &config.HardLimits{}, nil); err == nil {
		t.Fatal("一覧が無いのに読めたことにした")
	}
}

// 起動経路は env で受ける(フラグにすると、ビルドに失敗した日に古いバイナリが未知のフラグで落ちる)。
// 知らない値・空は手動として残す。
func TestTriggerFromEnv(t *testing.T) {
	for in, want := range map[string]string{"launchd": "launchd", "catchup": "catchup", "button": "button",
		"": "manual", "cron": "manual"} {
		if got := triggerFromEnv(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
