package command

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
)

// LLM が無くなっても **Promote 経路は残す**。決定論の
// テンプレートにも「メニュー外の戦略」「live_config 混入」「holding_mode 空」
// 「margin_oneday」のような事故はコードのバグとして起こりうる。入力を LLM の YAML から
// テンプレートに変えるだけ。

func promoteTmpl(t *testing.T, name config.StrategyName) *config.StrategyConfig {
	t.Helper()
	cfg, err := newTmpl().Build("7203", name, 2000, tmplNow)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return cfg
}

func TestPromoteConfigAcceptsTheDeterministicTemplate(t *testing.T) {
	for _, n := range []config.StrategyName{
		config.StrategyBNFReversion, config.StrategyBNFReversionTrail, config.StrategyBNFIntradayReversion,
		config.StrategyPostJumpDrift, config.StrategyHighVolumePremium, config.StrategyHigh52wMomentum,
		config.StrategyAbsMomentumV2, config.StrategyATRBreakoutV2, config.StrategyDonchianBreakoutV2,
	} {
		p := &Promoter{HardLimits: tmplHardLimits(), ExpectedSymbol: "7203", ExpectedStrategy: n}
		if _, err := p.PromoteConfig(promoteTmpl(t, n)); err != nil {
			t.Errorf("%s: テンプレートが promote を通らない: %v", n, err)
		}
	}
}

// 4 戦略は `direction: both`。**買い専用ゲートのままだとエラーも出さずに
// 売り標本ゼロ**になる(売りシグナルが全部 direction ゲートで落ちる)。
func TestPromoteConfigAcceptsThePreRegisteredDirection(t *testing.T) {
	for _, n := range []config.StrategyName{
		config.StrategyAbsMomentumV2, config.StrategyATRBreakoutV2,
		config.StrategyDonchianBreakoutV2, config.StrategyHigh52wMomentum,
	} {
		cfg := promoteTmpl(t, n)
		if cfg.Entry.Direction != config.DirectionBoth {
			t.Fatalf("%s: テンプレートの direction = %q, want both", n, cfg.Entry.Direction)
		}
		p := &Promoter{HardLimits: tmplHardLimits(), ExpectedSymbol: "7203", ExpectedStrategy: n}
		if _, err := p.PromoteConfig(cfg); err != nil {
			t.Errorf("%s: direction both が reject された: %v", n, err)
		}
	}
}

// 🛑 事前登録に無い向きは通さない。long-only 戦略(BNF)に both を渡すのは
// コードのバグであって、通すと台帳 #18 で reject 済みの鏡像が標本に混ざる。
func TestPromoteConfigRejectsADirectionThatIsNotPreRegistered(t *testing.T) {
	cfg := promoteTmpl(t, config.StrategyBNFReversion)
	cfg.Entry.Direction = config.DirectionBoth
	p := &Promoter{HardLimits: tmplHardLimits(), ExpectedSymbol: "7203"}
	if _, err := p.PromoteConfig(cfg); err == nil {
		t.Fatal("long-only 戦略に both が通った")
	}
	cfg.Entry.Direction = config.DirectionSellOnly
	if _, err := p.PromoteConfig(cfg); err == nil {
		t.Fatal("long-only 戦略に sell_only が通った")
	}
}

// buy_only は**常に通す**。事前登録が both の戦略でも、買いだけに絞るのは
// 空売りを開かない方向の縮退なので安全側(LLM 経路の契約もこれ)。
func TestPromoteConfigAlwaysAcceptsBuyOnly(t *testing.T) {
	cfg := promoteTmpl(t, config.StrategyAbsMomentumV2)
	cfg.Entry.Direction = config.DirectionBuyOnly
	p := &Promoter{HardLimits: tmplHardLimits(), ExpectedSymbol: "7203"}
	if _, err := p.PromoteConfig(cfg); err != nil {
		t.Fatalf("buy_only が reject された: %v", err)
	}
}

// 事故はコードのバグとして起こりうるので、既存の検証は全部残す。
func TestPromoteConfigStillRejectsTheKnownAccidents(t *testing.T) {
	base := func() *config.StrategyConfig { return promoteTmpl(t, config.StrategyAbsMomentumV2) }
	p := &Promoter{HardLimits: tmplHardLimits(), ExpectedSymbol: "7203"}

	live := base()
	live.Mode = config.ModeLive
	if _, err := p.PromoteConfig(live); err == nil {
		t.Error("live_config が通った")
	}
	empty := base()
	empty.HoldingMode = ""
	if _, err := p.PromoteConfig(empty); err == nil {
		t.Error("holding_mode 空が通った(引け前フラット化が死ぬ)")
	}
	oneday := base()
	oneday.ExecKind = order.ExecMarginOneday
	if _, err := p.PromoteConfig(oneday); err == nil {
		t.Error("margin_oneday が通った(立花 非対応)")
	}
	offMenu := base()
	offMenu.StrategyName = config.StrategyBNFEuphoriaShort
	if _, err := p.PromoteConfig(offMenu); err == nil {
		t.Error("メニュー外の戦略が通った")
	}
	wrongSym := base()
	wrongSym.Symbol = "6758"
	if _, err := p.PromoteConfig(wrongSym); err == nil {
		t.Error("別銘柄の config が通った")
	}
	zeroQty := base()
	zeroQty.Risk.Quantity = 0
	if _, err := p.PromoteConfig(zeroQty); err == nil {
		t.Error("quantity 0 が通った")
	}
}
