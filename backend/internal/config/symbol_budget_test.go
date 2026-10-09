package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSymbolBudget_ParsedFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot_config.yaml")
	yaml := "mode: paper_config\nbroker: { kind: paper_live_feed }\nsymbols: [\"7203\"]\n" +
		"risk:\n  account_max_open_symbols: 80\n  per_entry_max_open_symbols: 12\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Risk.AccountMaxOpenSymbols != 80 || cfg.Risk.PerEntryMaxOpenSymbols != 12 {
		t.Fatalf("account=%d per_entry=%d, want 80 / 12",
			cfg.Risk.AccountMaxOpenSymbols, cfg.Risk.PerEntryMaxOpenSymbols)
	}
}

// 既定は 0 = 無効。paper 以外(backtest / テスト配線)の挙動を 1 ビットも変えない。
func TestSymbolBudget_DefaultsToDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot_config.yaml")
	if err := os.WriteFile(path, []byte("mode: paper_config\nbroker: { kind: paper }\nsymbols: [\"7203\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Risk.AccountMaxOpenSymbols != 0 || cfg.Risk.PerEntryMaxOpenSymbols != 0 {
		t.Fatalf("既定は無効でなければならない: account=%d per_entry=%d",
			cfg.Risk.AccountMaxOpenSymbols, cfg.Risk.PerEntryMaxOpenSymbols)
	}
}

// 🛑 研究モードの設定を実弾に持ち込まない前例(全トリガー採用 /
// account_max_open_positions)に揃える。live の建玉枠は `account_max_open_positions`
// (現行 4)で、銘柄予算は紙トラックの通信・解像度の道具。
func TestSymbolBudget_RejectedForLiveMode(t *testing.T) {
	hl := &HardLimits{AllowedSymbols: []string{"7203"}}
	for _, tc := range []struct {
		name string
		mut  func(*BotConfig)
	}{
		{"account_max_open_symbols", func(c *BotConfig) { c.Risk.AccountMaxOpenSymbols = 80 }},
		{"per_entry_max_open_symbols", func(c *BotConfig) { c.Risk.PerEntryMaxOpenSymbols = 12 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &BotConfig{Mode: ModeLive, Broker: BrokerCfg{Kind: BrokerTachibana}, Symbols: []string{"7203"}}
			tc.mut(cfg)
			err := cfg.ValidateAgainstHardLimits(hl)
			if err == nil {
				t.Fatalf("live_config が %s を持ててはいけない", tc.name)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("エラーは原因を名指ししなければならない: %v", err)
			}
		})
	}
}
