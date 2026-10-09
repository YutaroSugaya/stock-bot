package main

import (
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/usecase/command"
)

// 未登録の戦略を active config が名指しすると起動時に落ちる。
func TestLiveEngineRegistersEdgeCandidates(t *testing.T) {
	eng := buildStrategyEngine(func() string { return "x" })
	for _, name := range []config.StrategyName{
		config.StrategyNoTrade,
		config.StrategyBNFReversion,
		config.StrategyBNFReversionTrail,
		config.StrategyBNFIntradayReversion,
		config.StrategyBNFIntradayReversionTrail,
		config.StrategyHighVolumePremium,
		config.StrategyPostJumpDrift,
	} {
		if !eng.Has(name) {
			t.Fatalf("live engine missing strategy %q (registered: %v)", name, eng.Names())
		}
	}
}

// commit 済みの example config が起動経路をそのまま通ること(サーバは立てない)。
func TestBNFExampleConfigBootable(t *testing.T) {
	const root = "../../../configs/"
	hl, err := config.LoadHardLimits(root + "hard_limits.yaml")
	if err != nil {
		t.Fatalf("load hard limits: %v", err)
	}
	cfg, err := config.LoadStrategyConfig(root + "strategy_config.bnf.example.yaml")
	if err != nil {
		t.Fatalf("load BNF example config: %v", err)
	}
	if cfg.StrategyName != config.StrategyBNFReversion {
		t.Fatalf("strategy_name = %q, want bnf_reversion", cfg.StrategyName)
	}
	// "*" universe config は銘柄ごとに展開してから hard_limits を当てる。
	cand := configForSymbol(cfg, "7203")
	if cand == nil {
		t.Fatal("configForSymbol returned nil for a universe config")
	}
	if err := cand.ValidateAgainstHardLimits(hl); err != nil {
		t.Fatalf("expanded BNF config rejected by hard limits: %v", err)
	}
	if !buildStrategyEngine(func() string { return "x" }).Has(cand.StrategyName) {
		t.Fatalf("live engine does not register %q", cand.StrategyName)
	}
}

// 日中テンプレの起動経路。DAY-trade である条件(intraday holding + 立花が実際に
// 執行できる exec_kind。一日信用は非対応)も固定する。
func TestBNFIntradayExampleConfigBootable(t *testing.T) {
	const root = "../../../configs/"
	hl, err := config.LoadHardLimits(root + "hard_limits.yaml")
	if err != nil {
		t.Fatalf("load hard limits: %v", err)
	}
	cfg, err := config.LoadStrategyConfig(root + "strategy_config.bnf_intraday.example.yaml")
	if err != nil {
		t.Fatalf("load BNF intraday example config: %v", err)
	}
	if cfg.StrategyName != config.StrategyBNFIntradayReversion {
		t.Fatalf("strategy_name = %q, want bnf_intraday_reversion", cfg.StrategyName)
	}
	if cfg.HoldingMode != order.HoldingIntraday {
		t.Fatalf("holding_mode = %q, want intraday (day-trade template)", cfg.HoldingMode)
	}
	if !config.BrokerTachibana.SupportsExecKind(cfg.ExecKind) {
		t.Fatalf("exec_kind %q is not executable on tachibana (一日信用非対応)", cfg.ExecKind)
	}
	cand := configForSymbol(cfg, "7203")
	if cand == nil {
		t.Fatal("configForSymbol returned nil for a universe config")
	}
	if err := cand.ValidateAgainstHardLimits(hl); err != nil {
		t.Fatalf("expanded config rejected by hard limits: %v", err)
	}
	if !buildStrategyEngine(func() string { return "x" }).Has(cand.StrategyName) {
		t.Fatalf("live engine does not register %q", cand.StrategyName)
	}
}

func TestConfigForSymbol(t *testing.T) {
	wild := &config.StrategyConfig{ConfigID: "bnf_v1", Symbol: "*", StrategyName: config.StrategyBNFReversion}
	a := configForSymbol(wild, "7203")
	b := configForSymbol(wild, "6758")
	if a == nil || b == nil {
		t.Fatal("universe config should expand to every symbol")
	}
	if a.Symbol != "7203" || b.Symbol != "6758" {
		t.Fatalf("expanded symbols = %q,%q", a.Symbol, b.Symbol)
	}
	if a.ConfigID == b.ConfigID {
		t.Fatalf("expanded config_ids must be unique (got %q for both)", a.ConfigID)
	}
	if wild.Symbol != "*" {
		t.Fatal("expansion must not mutate the base config")
	}

	specific := &config.StrategyConfig{ConfigID: "x", Symbol: "7203"}
	if got := configForSymbol(specific, "7203"); got != specific {
		t.Fatal("specific config should be used as-is for its symbol")
	}
	if got := configForSymbol(specific, "6758"); got != nil {
		t.Fatal("specific config must not apply to a different symbol")
	}
}

// 名前の登録だけでなく、bot と同じ engine が実際に BNF へ dispatch して建てること。
func TestLiveEngineDispatchesBNF(t *testing.T) {
	eng := buildStrategyEngine(func() string { return "sig-live-1" })
	cfg := &config.StrategyConfig{
		ConfigID: "bnf_paper", Symbol: "7203", StrategyName: config.StrategyBNFReversion,
		HoldingMode: order.HoldingMultiday,
	}
	cfg.Entry.Direction = config.DirectionBuyOnly
	cfg.Risk.Quantity = 100

	// 29 calm bars then a -15% crash on 2.2x volume = BNF panic trigger.
	cs := make([]market.Candle, 30)
	for i := 0; i < 29; i++ {
		cs[i] = market.Candle{Open: 2000, High: 2010, Low: 1990, Close: 2000, Volume: 1000}
	}
	cs[29] = market.Candle{Open: 1990, High: 2000, Low: 1690, Close: 1700, Volume: 2200}

	in := strategy.EvalInput{
		Now: time.Now(), Config: cfg, CandlesDaily: cs,
		Summary: &market.MarketSummary{
			Symbol: "7203", TickSize: 1,
			CurrentRate: market.CurrentRate{Last: 1700, SpreadTicks: 1},
		},
	}
	sig := eng.Evaluate(in)
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Fatalf("live engine should dispatch bnf_reversion and BUY the crash, got decision=%q side=%q reason=%q",
			sig.Decision, sig.Side, sig.Reason)
	}
	if sig.SignalID == "" {
		t.Fatal("engine should stamp a SignalID on the entry")
	}
}

// bot の engine に載るのは**メニューと no_trade だけ**(D-5)。メニューの全戦略が載っていること
// (menu と screener の一致は戦略カタログが構造で保ち、catalog_test が縛る)。v2 の
// 3 本が engine に無く、枠を消費した末に Arm が "unregistered strategy" で落ちた。逆に棄却済み・
// v1 の戦略は engine に載せない — 実装は cmd/backtest の再現用にカタログに残っている。
func TestEngineRegistersExactlyTheMenu(t *testing.T) {
	eng := buildStrategyEngine(func() string { return "x" })
	want := map[string]bool{string(config.StrategyNoTrade): true}
	for _, name := range command.AdvisorCandidateStrategies {
		want[string(name)] = true
		if !eng.Has(name) {
			t.Errorf("menu strategy %q は engine 未登録 — arm で unregistered として弾かれ、枠だけ溶ける", name)
		}
	}
	for _, name := range eng.Names() {
		if !want[name] {
			t.Errorf("engine にメニュー外の %q が載っている(棄却済み・v1 は backtest の再現用だけ)", name)
		}
	}
}
