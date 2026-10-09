package command

import (
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
)

// advisor(LLM)を arm 経路から外し、決定論のテンプレートに置き換える。
// 実測(直近 200 run 全件)で **実行に効く変動する LLM 出力は 1 つも無かった** — 生きた
// 出力は `strategy_name` の 1 ビットだけで、それも 97% が「構える」だった。
// テンプレートの表がそのまま本テンプレートの仕様になる。

func tmplHardLimits() *config.HardLimits {
	hl := &config.HardLimits{}
	hl.Quantity = config.IntRange{Min: 100, Max: 3000}
	hl.MaxTradesInThisWindow = config.IntRange{Min: 1, Max: 5}
	hl.MaxLossInWindowJPY = config.IntRange{Min: 1000, Max: 1000000}
	hl.MaxSpreadTicks = config.FloatRange{Min: 1, Max: 10}
	hl.ConfigTTLMinutes = config.IntRange{Min: 60, Max: 120}
	hl.AllowedSymbols = []string{"7203", "6758"}
	return hl
}

func newTmpl() *ArmTemplate {
	return &ArmTemplate{
		HardLimits: tmplHardLimits(),
		Mode:       config.ModePaper,
		ExecKindFor: func(m order.HoldingMode) order.ExecKind {
			if m == order.HoldingIntraday {
				return order.ExecCash
			}
			return order.ExecMarginSystem
		},
	}
}

var tmplNow = time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)

// 表: 変動しない項目は全部定数。
func TestArmTemplateUsesThePreRegisteredConstants(t *testing.T) {
	cfg, err := newTmpl().Build("7203", config.StrategyDonchianBreakoutV2, 2000, tmplNow)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if cfg.Symbol != "7203" || cfg.StrategyName != config.StrategyDonchianBreakoutV2 {
		t.Fatalf("symbol/strategy が違う: %+v", cfg)
	}
	if cfg.Mode != config.ModePaper {
		t.Fatalf("mode = %q, want paper_config", cfg.Mode)
	}
	if cfg.Risk.Quantity != 100 || cfg.Risk.MaxOpenPositions != 1 || cfg.Risk.MaxTradesInThisWindow != 3 {
		t.Fatalf("risk 定数が違う: %+v", cfg.Risk)
	}
	if cfg.Entry.MaxSpreadTicks != 5 {
		t.Fatalf("max_spread_ticks = %v, want 5", cfg.Entry.MaxSpreadTicks)
	}
	if cfg.TTLMinutes != 0 {
		t.Fatalf("ttl_minutes = %d, want 0", cfg.TTLMinutes)
	}
	// 出口は戦略側が ATR から算出する。config の円幅・ratchet・extension は全部 0。
	if cfg.Exit.TakeProfitJPY != 0 || cfg.Exit.StopLossJPY != 0 ||
		cfg.Exit.RatchetArmJPY != 0 || cfg.Exit.RatchetGivebackJPY != 0 ||
		cfg.Exit.ExtensionMaxMinutes != 0 || cfg.Exit.EarlyExitWindowMinutes != 0 ||
		cfg.Exit.MaxHoldMinutes != 0 {
		t.Fatalf("exit ブロックが 0 でない: %+v", cfg.Exit)
	}
}

// 唯一の式: max_loss_in_this_window_jpy = 直近終値 × 0.07 × quantity × 3。
// 🛑 `stop_loss_jpy` から計算しない — 実際の1敗は ATR 由来(2.0×ATR ≈ 終値比 7%)で、
// 旧式だと窓上限が実際の1敗より小さくなり **3敗ぶんのつもりが1敗で発動**する。
func TestArmTemplateWindowLossIsThreeATRLosses(t *testing.T) {
	cfg, err := newTmpl().Build("7203", config.StrategyAbsMomentumV2, 2000, tmplNow)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if want := int(2000 * 0.07 * 100 * 3); cfg.Risk.MaxLossInThisWindowJPY != want {
		t.Fatalf("max_loss_in_this_window_jpy = %d, want %d", cfg.Risk.MaxLossInThisWindowJPY, want)
	}
}

// 極端に安い銘柄で式の値が hard limit の下限を割ると、config ごと reject されて
// **標本が静かに落ちる**。下限へ寄せる(上へ寄せるので cap は緩む = censoring しない)。
func TestArmTemplateClampsWindowLossToTheHardLimitFloor(t *testing.T) {
	cfg, err := newTmpl().Build("7203", config.StrategyAbsMomentumV2, 40, tmplNow)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if cfg.Risk.MaxLossInThisWindowJPY != 1000 {
		t.Fatalf("下限に寄せていない: %d", cfg.Risk.MaxLossInThisWindowJPY)
	}
	if err := cfg.ValidateAgainstHardLimits(tmplHardLimits()); err != nil {
		t.Fatalf("hard limits を通らない config を作った: %v", err)
	}
}

// 分岐は 1 つだけ: bnf_intraday_reversion(とその `_trail` 兄弟)は intraday、それ以外は multiday。
func TestArmTemplateHoldingModeHasExactlyOneBranch(t *testing.T) {
	for _, n := range []config.StrategyName{config.StrategyBNFIntradayReversion, config.StrategyBNFIntradayReversionTrail} {
		intra, _ := newTmpl().Build("7203", n, 2000, tmplNow)
		if intra.HoldingMode != order.HoldingIntraday {
			t.Fatalf("%s = %q, want intraday", n, intra.HoldingMode)
		}
		if intra.ExecKind != order.ExecCash {
			t.Fatalf("%s exec_kind = %q, want cash(立花は一日信用 非対応)", n, intra.ExecKind)
		}
	}
	for _, n := range []config.StrategyName{
		config.StrategyBNFReversion, config.StrategyAbsMomentumV2, config.StrategyHigh52wMomentum,
	} {
		c, _ := newTmpl().Build("7203", n, 2000, tmplNow)
		if c.HoldingMode != order.HoldingMultiday {
			t.Errorf("%s = %q, want multiday", n, c.HoldingMode)
		}
	}
}

// 🛑 exec_kind は **実際に執行される区分**をそのまま凍結する。テンプレートの表が「全戦略 cash」
// と書いていたが、執行は holding_mode から解決される(multiday = margin_system)ので、
// cash と書くと台帳の凍結値だけが嘘になる。売りは現物では出せないので必須。
func TestArmTemplateFreezesTheExecKindThatWillActuallyBeUsed(t *testing.T) {
	multi, _ := newTmpl().Build("7203", config.StrategyAbsMomentumV2, 2000, tmplNow)
	if multi.ExecKind != order.ExecMarginSystem {
		t.Fatalf("multiday の exec_kind = %q, want margin_system", multi.ExecKind)
	}
	intra, _ := newTmpl().Build("7203", config.StrategyBNFIntradayReversion, 2000, tmplNow)
	if intra.ExecKind != order.ExecCash {
		t.Fatalf("intraday の exec_kind = %q, want cash", intra.ExecKind)
	}
}

// 同じ入力なら**同じ config_id**(再現性)。日と内容が変われば別 id。
func TestArmTemplateConfigIDIsDeterministicAndContentBound(t *testing.T) {
	a, _ := newTmpl().Build("7203", config.StrategyAbsMomentumV2, 2000, tmplNow)
	b, _ := newTmpl().Build("7203", config.StrategyAbsMomentumV2, 2000, tmplNow.Add(3*time.Hour))
	if a.ConfigID != b.ConfigID {
		t.Fatalf("同じ日・同じ内容で config_id が揺れた: %q vs %q", a.ConfigID, b.ConfigID)
	}
	if !strings.HasPrefix(a.ConfigID, "det-") {
		t.Fatalf("config_id に決定論 arm の目印が無い: %q", a.ConfigID)
	}
	// 内容が変われば id も変わる(config 凍結: 同じ id は永久に同じ内容)。
	c, _ := newTmpl().Build("7203", config.StrategyAbsMomentumV2, 2500, tmplNow)
	if a.ConfigID == c.ConfigID {
		t.Fatalf("内容が違うのに同じ config_id: %q", a.ConfigID)
	}
	// 戦略が違えば別 id(1 銘柄に複数 active が載るので衝突する)。
	d, _ := newTmpl().Build("7203", config.StrategyDonchianBreakoutV2, 2000, tmplNow)
	if a.ConfigID == d.ConfigID {
		t.Fatalf("別戦略なのに同じ config_id: %q", a.ConfigID)
	}
}

// LLM の痕跡を残さない(advisor_run_id は runtime が刻む provenance)。
func TestArmTemplateCarriesNoAdvisorProvenance(t *testing.T) {
	cfg, _ := newTmpl().Build("7203", config.StrategyAbsMomentumV2, 2000, tmplNow)
	if cfg.AdvisorRunID != "" {
		t.Fatalf("advisor_run_id が入っている: %q", cfg.AdvisorRunID)
	}
	if cfg.MarketRegime.Reason != "" || cfg.MarketRegime.Type != "" {
		t.Fatalf("market_regime を捏造している: %+v", cfg.MarketRegime)
	}
}

// 価格が取れない銘柄は fail-close(窓上限を検証できない config を作らない)。
func TestArmTemplateRefusesWithoutAPrice(t *testing.T) {
	if _, err := newTmpl().Build("7203", config.StrategyAbsMomentumV2, 0, tmplNow); err == nil {
		t.Fatal("直近終値 0 で config を作った")
	}
}

// hard limits を通らない config を返さない(呼び手が arm できない config を掴まない)。
func TestArmTemplateOutputPassesHardLimits(t *testing.T) {
	for _, n := range []config.StrategyName{
		config.StrategyBNFReversion, config.StrategyBNFReversionTrail, config.StrategyBNFIntradayReversion,
		config.StrategyPostJumpDrift, config.StrategyHighVolumePremium, config.StrategyHigh52wMomentum,
		config.StrategyAbsMomentumV2, config.StrategyATRBreakoutV2, config.StrategyDonchianBreakoutV2,
	} {
		cfg, err := newTmpl().Build("7203", n, 2000, tmplNow)
		if err != nil {
			t.Fatalf("%s: build: %v", n, err)
		}
		if err := cfg.ValidateAgainstHardLimits(tmplHardLimits()); err != nil {
			t.Errorf("%s: hard limits: %v", n, err)
		}
	}
}
