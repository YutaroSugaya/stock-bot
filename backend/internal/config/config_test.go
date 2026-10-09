package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const hardLimitsYAML = `
allowed_symbols: ["7203", "6758"]
quantity: { min: 100, max: 3000 }
take_profit_jpy: { min: 1, max: 200 }
stop_loss_jpy: { min: 1, max: 100 }
max_hold_minutes: { min: 5, max: 360 }
max_trades_in_this_window: { min: 1, max: 5 }
max_loss_in_this_window_jpy: { min: 1000, max: 1000000 }
max_spread_ticks: { min: 1, max: 10 }
config_ttl_minutes: { min: 60, max: 120 }
margin: { min_collateral_jpy: 300000, required_rate: 0.30, maintenance_ratio: 0.30 }
session_hours:
  timezone: "Asia/Tokyo"
  sessions:
    - { start: "09:00", end: "11:30" }
    - { start: "12:30", end: "15:00" }
  entry_cutoff: "14:55"
  force_flat_at: "14:50"
`

func TestLoadHardLimits(t *testing.T) {
	hl, err := LoadHardLimits(writeTemp(t, "hard_limits.yaml", hardLimitsYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !hl.AllowsSymbol("7203") || hl.AllowsSymbol("9999") {
		t.Fatal("symbol whitelist wrong")
	}
	if !hl.Quantity.Contains(100) || hl.Quantity.Contains(5000) {
		t.Fatal("quantity range wrong")
	}
	th, err := hl.SessionHours.TradingHours()
	if err != nil || len(th.Sessions) != 2 {
		t.Fatalf("session hours: %v %+v", err, th)
	}
}

// range を 1 本でも書き忘れた hard_limits は**読み込み時に落とす**。
// ValidateAgainstHardLimits は Max==0 を「未宣言 = 縛らない」と解釈するので、
// ここで塞がないとその項目だけ静かに無制限になる(committed yaml だけでなく
// STOCKBOT_HARD_LIMITS で差し替えた任意のファイルにも効かせる必要がある)。
func TestLoadHardLimits_RejectsMissingRange(t *testing.T) {
	for _, key := range []string{
		"quantity", "take_profit_jpy", "stop_loss_jpy", "max_hold_minutes",
		"max_trades_in_this_window", "max_loss_in_this_window_jpy",
		"max_spread_ticks", "config_ttl_minutes",
	} {
		var b strings.Builder
		for _, line := range strings.Split(strings.TrimSpace(hardLimitsYAML), "\n") {
			if strings.HasPrefix(line, key+":") {
				continue
			}
			b.WriteString(line + "\n")
		}
		if _, err := LoadHardLimits(writeTemp(t, "hl.yaml", b.String())); err == nil {
			t.Errorf("%s を落とした hard_limits が読めてしまう(その項目だけ無制限になる)", key)
		}
	}
}

func TestLoadHardLimits_RejectsInvalid(t *testing.T) {
	bad := `
allowed_symbols: []
quantity: { min: 100, max: 3000 }
session_hours:
  timezone: "Asia/Tokyo"
  sessions: [{ start: "09:00", end: "11:30" }]
`
	if _, err := LoadHardLimits(writeTemp(t, "bad.yaml", bad)); err == nil {
		t.Fatal("empty allowed_symbols must fail (fail-close)")
	}
}

func TestBotConfig_ValidateSymbolWhitelist(t *testing.T) {
	hl, _ := LoadHardLimits(writeTemp(t, "hard_limits.yaml", hardLimitsYAML))

	good := writeTemp(t, "bot.yaml", "mode: paper_config\nbroker: { kind: paper }\nsymbols: [\"7203\"]\n")
	bc, err := LoadBotConfig(good)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := bc.ValidateAgainstHardLimits(hl); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	bad := writeTemp(t, "bot2.yaml", "mode: paper_config\nbroker: { kind: paper }\nsymbols: [\"9999\"]\n")
	bc2, _ := LoadBotConfig(bad)
	if err := bc2.ValidateAgainstHardLimits(hl); err == nil {
		t.Fatal("non-whitelisted symbol must fail-close")
	}
}

func TestLoadBotConfig_RejectsUnknownMode(t *testing.T) {
	// A typo'd mode must fail closed at load, not fall through as "not live".
	bad := writeTemp(t, "bot.yaml", "mode: live_configg\nbroker: { kind: tachibana }\nsymbols: [\"7203\"]\n")
	if _, err := LoadBotConfig(bad); err == nil {
		t.Fatal("an unrecognised mode must be rejected at load (fail-close)")
	}
	// Empty mode still defaults to paper.
	empty := writeTemp(t, "bot2.yaml", "broker: { kind: paper }\nsymbols: [\"7203\"]\n")
	bc, err := LoadBotConfig(empty)
	if err != nil || bc.Mode != ModePaper {
		t.Fatalf("empty mode must default to paper, got mode=%q err=%v", bc.Mode, err)
	}
	if !ModeLive.Valid() || !ModePaper.Valid() || !ModeDisabled.Valid() || Mode("nope").Valid() {
		t.Fatal("Mode.Valid must accept the three known modes and reject others")
	}
}

func TestBotConfig_LiveRequiresRealBroker(t *testing.T) {
	hl, _ := LoadHardLimits(writeTemp(t, "hard_limits.yaml", hardLimitsYAML))
	p := writeTemp(t, "bot.yaml", "mode: live_config\nbroker: { kind: paper }\nsymbols: [\"7203\"]\n")
	bc, _ := LoadBotConfig(p)
	if err := bc.ValidateAgainstHardLimits(hl); err == nil {
		t.Fatal("live mode with paper broker must be rejected")
	}
}

func TestBotConfig_BrokerCapabilities(t *testing.T) {
	// 立花 + 一日信用 (margin_oneday) must be rejected — the e支店 API has no 一日信用.
	tachiOneday := writeTemp(t, "bot.yaml", "broker: { kind: tachibana }\nsymbols: [\"7203\"]\nholding:\n  multiday: { exec_kind: cash }\n  intraday: { exec_kind: margin_oneday }\n")
	bc, _ := LoadBotConfig(tachiOneday)
	if err := bc.ValidateBrokerCapabilities(); err == nil {
		t.Fatal("tachibana with 一日信用 (margin_oneday) must be rejected")
	}

	// 立花 + cash and 立花 + 一般信用 (margin_general) are both accepted.
	for _, ek := range []string{"cash", "margin_system"} {
		p := writeTemp(t, "bot.yaml", "broker: { kind: tachibana }\nsymbols: [\"7203\"]\nholding:\n  multiday: { exec_kind: "+ek+" }\n  intraday: { exec_kind: cash }\n")
		bc, _ := LoadBotConfig(p)
		if err := bc.ValidateBrokerCapabilities(); err != nil {
			t.Fatalf("tachibana with exec_kind %q should pass: %v", ek, err)
		}
	}

	// paper may use any margin kind (simulation).
	paperMargin := writeTemp(t, "bot.yaml", "broker: { kind: paper }\nsymbols: [\"7203\"]\nholding:\n  multiday: { exec_kind: margin_general }\n  intraday: { exec_kind: margin_oneday }\n")
	bc3, _ := LoadBotConfig(paperMargin)
	if err := bc3.ValidateBrokerCapabilities(); err != nil {
		t.Fatalf("paper with margin exec_kind should pass: %v", err)
	}
}

func TestRequireLiveGuards(t *testing.T) {
	t.Setenv("STOCKBOT_LIVE_CONFIRMED", "")
	if err := RequireLiveGuards(ModeLive); err == nil {
		t.Fatal("live without confirmation env must fail")
	}
	t.Setenv("STOCKBOT_LIVE_CONFIRMED", "1")
	if err := RequireLiveGuards(ModeLive); err != nil {
		t.Fatalf("live with confirmation should pass: %v", err)
	}
	if err := RequireLiveGuards(ModePaper); err != nil {
		t.Fatalf("paper should always pass: %v", err)
	}
}

// hard_limits.yaml に書いた range は**書いただけでは効かない**。LLM が生成した
// strategy config は advisor_promote 経由で ValidateAgainstHardLimits だけを通るので、
// ここで束縛していない範囲は「hard limit に書いてあるのに素通り」になる。
func TestStrategyConfig_HardLimitsBindEveryDeclaredRange(t *testing.T) {
	hl, err := LoadHardLimits(writeTemp(t, "hard_limits.yaml", hardLimitsYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	base := func() *StrategyConfig {
		c := &StrategyConfig{Symbol: "7203", TTLMinutes: 90}
		c.Entry.MaxSpreadTicks = 5
		c.Risk.MaxTradesInThisWindow = 3
		c.Risk.MaxLossInThisWindowJPY = 8000
		return c
	}
	if err := base().ValidateAgainstHardLimits(hl); err != nil {
		t.Fatalf("範囲内の config が拒否された: %v", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*StrategyConfig)
	}{
		{"max_trades_in_this_window", func(c *StrategyConfig) { c.Risk.MaxTradesInThisWindow = 50 }},
		{"max_loss_in_this_window_jpy", func(c *StrategyConfig) { c.Risk.MaxLossInThisWindowJPY = 9_000_000 }},
		{"max_spread_ticks", func(c *StrategyConfig) { c.Entry.MaxSpreadTicks = 999 }},
		{"config_ttl_minutes", func(c *StrategyConfig) { c.TTLMinutes = 100000 }},
	} {
		c := base()
		tc.mut(c)
		if err := c.ValidateAgainstHardLimits(hl); err == nil {
			t.Errorf("%s: hard limit の範囲外なのに素通りした(宣言だけで拘束していない)", tc.name)
		}
	}

	// 0 = 未設定は既存の他フィールドと同じく素通し(examples の ttl_minutes: 0 を壊さない)。
	z := base()
	z.TTLMinutes, z.Entry.MaxSpreadTicks, z.Risk.MaxTradesInThisWindow, z.Risk.MaxLossInThisWindowJPY = 0, 0, 0, 0
	if err := z.ValidateAgainstHardLimits(hl); err != nil {
		t.Fatalf("未設定(0)は素通しであること: %v", err)
	}
}

func TestStrategyConfig_Expiry(t *testing.T) {
	now := time.Now()
	c := &StrategyConfig{TTLMinutes: 60}
	// no ActivatedAt -> never expires
	if c.IsExpired(now) {
		t.Fatal("config without ActivatedAt should not expire")
	}
	c.ActivatedAt = now.Add(-2 * time.Hour)
	if !c.IsExpired(now) {
		t.Fatal("config 2h past a 60m TTL should be expired")
	}
}
