package command

import (
	"strings"
	"testing"

	"stockbot/backend/internal/config"
)

func testHardLimits() *config.HardLimits {
	return &config.HardLimits{
		AllowedSymbols: []string{"7203"},
		Quantity:       config.IntRange{Min: 1, Max: 1000},
		TakeProfitJPY:  config.FloatRange{Min: 1, Max: 1000},
		StopLossJPY:    config.FloatRange{Min: 1, Max: 1000},
		MaxHoldMinutes: config.IntRange{Min: 0, Max: 100000},
	}
}

const bnfPaperYAML = `config_id: t1
symbol: "7203"
strategy_name: bnf_reversion
mode: paper_config
holding_mode: multiday
exec_kind: cash
ttl_minutes: 0
entry:
  direction: buy_only
  max_spread_ticks: 5
exit:
  take_profit_jpy: 0
  stop_loss_jpy: 0
risk:
  quantity: 100
  max_open_positions: 1
`

func TestPromote_ValidBNFPaper(t *testing.T) {
	p := &Promoter{HardLimits: testHardLimits(), ExpectedSymbol: "7203"}
	cfg, err := p.Promote([]byte(bnfPaperYAML))
	if err != nil {
		t.Fatalf("unexpected reject: %v", err)
	}
	if cfg.StrategyName != config.StrategyBNFReversion || cfg.Mode != config.ModePaper {
		t.Fatalf("bad cfg: %+v", cfg)
	}
}

// mustReject は「Promote が error を返し、その文言が want を含む」ことを検査する。
// 4 行 × 11 箇所の手写しを 1 行にした。ctx は失敗時にどのケースか分かるようにする
// ための必須引数(ループ内 4 箇所は元から名前を出していた)。
// t.Fatalf ではなく t.Errorf — ループ内の非 fatal 挙動を保つため。
func mustReject(t *testing.T, p *Promoter, y []byte, want, ctx string) {
	t.Helper()
	_, err := p.Promote(y)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: reject されるべき(文言に %q を含むこと)。got %v", ctx, want, err)
	}
}

func TestPromote_RejectsLiveMode(t *testing.T) {
	y := strings.Replace(bnfPaperYAML, "mode: paper_config", "mode: live_config", 1)
	mustReject(t, &Promoter{HardLimits: testHardLimits()}, []byte(y), "paper_config only", "live mode")
}

// LLM は `symbol: 7182` と引用符なし(YAML では整数)で出しがちで、yaml.v3 は
// !!int → string を拒否する。初稼働で全10銘柄の arm が「promote:
// parse yaml」で silent 全滅した。受けるのはスカラーの型ゆらぎだけ。
func TestPromote_AcceptsUnquotedNumericSymbol(t *testing.T) {
	y := strings.ReplaceAll(bnfPaperYAML, `symbol: "7203"`, `symbol: 7203`)
	cfg, err := (&Promoter{HardLimits: testHardLimits(), ExpectedSymbol: "7203"}).Promote([]byte(y))
	if err != nil {
		t.Fatalf("引用符なしの数値 symbol はパースできるべき(LLM の現実): %v", err)
	}
	if cfg.Symbol != "7203" {
		t.Fatalf("symbol = %q, want \"7203\"", cfg.Symbol)
	}
}

func TestPromote_RejectsOffMenuStrategy(t *testing.T) {
	y := strings.Replace(bnfPaperYAML, "strategy_name: bnf_reversion", "strategy_name: time_series_momentum", 1)
	mustReject(t, &Promoter{HardLimits: testHardLimits()}, []byte(y), "candidate menu", "off-menu strategy")
}

func TestPromote_RejectsSymbolMismatch(t *testing.T) {
	mustReject(t, &Promoter{HardLimits: testHardLimits(), ExpectedSymbol: "6758"}, []byte(bnfPaperYAML), "symbol mismatch", "symbol mismatch")
}

func TestPromote_RejectsQuantityOverHardLimit(t *testing.T) {
	y := strings.Replace(bnfPaperYAML, "quantity: 100", "quantity: 5000", 1)
	mustReject(t, &Promoter{HardLimits: testHardLimits()}, []byte(y), "quantity", "over-limit quantity")
}

// **SL 必須ゲートは撤去済み。**
//
// 以前ここは「config-exit 戦略で stop_loss_jpy=0 は reject」を固定していた。あれは
// 「プロンプトがまだ tp/sl を返す契約なので、値が欠けた config を『LLM が指示を無視した』
// シグナルとして弾く」ためのもので、**SL の実在を保証するものではなかった** —
// ATR 化以降、6戦略の実際の SL は 2.0×ATR で cfg.Exit の円幅は読まれない。
//
// SL の実在を今保証しているのは **`enterIfClears` の `no_atr` 分岐**(ATR が取れなければ
// 建てない。黙って円幅へ縮退する経路は存在しない)で、そちらは
// `strategy/atr_exit_test.go` が固定している。ここではゲートが**確かに外れた**こと、
// つまり決定論テンプレートの形(exit 全て 0)が通ることを固定する。
func TestPromote_ConfigExitStrategiesAcceptZeroExitBlock(t *testing.T) {
	for _, name := range []config.StrategyName{
		config.StrategyPostJumpDrift, config.StrategyHighVolumePremium,
		config.StrategyHigh52wMomentum, config.StrategyDonchianBreakoutV2,
		config.StrategyATRBreakoutV2, config.StrategyAbsMomentumV2,
	} {
		y := strings.Replace(bnfPaperYAML, "strategy_name: bnf_reversion", "strategy_name: "+string(name), 1)
		if _, err := (&Promoter{HardLimits: testHardLimits()}).Promote([]byte(y)); err != nil {
			t.Errorf("%s: exit 全て 0 のテンプレートが通らない: %v", name, err)
		}
	}
}

func TestPromote_BNFAllowsZeroConfigExit(t *testing.T) {
	if _, err := (&Promoter{HardLimits: testHardLimits()}).Promote([]byte(bnfPaperYAML)); err != nil {
		t.Fatalf("bnf_reversion with 0 config exit must pass (computes own exit), got %v", err)
	}
}

// direction はかつて prompt という soft constraint だけで、sell_only / both / 空 が
// 素通りしていた。daily 系戦略は sell 側も出すので、long-only 戦略に売りが混ざると
// 台帳 #18 で reject 済みの鏡像が forward 標本に入る。
//
// 「全部 buy_only」から「**戦略ごとの事前登録値**(ArmDirection)
// または buy_only」に変わった。bnf_reversion は long-only のままなので、この 3 つは
// 引き続き reject される。both が通るのは 4 戦略(abs / atr / donchian の v2 + high_52w)
// だけで、それは promote_template_test が別に固定する。
func TestPromote_RejectsNonBuyOnlyDirection(t *testing.T) {
	for _, dir := range []string{"direction: sell_only", "direction: both", "direction: \"\""} {
		y := strings.Replace(bnfPaperYAML, "direction: buy_only", dir, 1)
		mustReject(t, &Promoter{HardLimits: testHardLimits()}, []byte(y), "direction", dir+" (bnf_reversion は long-only)")
	}
	y := strings.Replace(bnfPaperYAML, "strategy_name: bnf_reversion", "strategy_name: no_trade", 1)
	y = strings.Replace(y, "direction: buy_only", "direction: \"\"", 1)
	if _, err := (&Promoter{HardLimits: testHardLimits()}).Promote([]byte(y)); err != nil {
		t.Errorf("no_trade with empty direction should pass, got %v", err)
	}
}

// An empty holding_mode flows to the Signal, makes exec-kind fall through to
// margin_oneday, and ForceFlatten then skips the position — a 一日信用 建玉 carried
// past the bell. The prompt alone is not enough.
func TestPromote_RejectsMissingOrInvalidHoldingMode(t *testing.T) {
	for _, bad := range []string{"", "swing", "INTRADAY"} {
		y := strings.Replace(bnfPaperYAML, "holding_mode: multiday", "holding_mode: "+bad, 1)
		if bad == "" {
			y = strings.Replace(bnfPaperYAML, "holding_mode: multiday\n", "", 1)
		}
		mustReject(t, &Promoter{HardLimits: testHardLimits()}, []byte(y), "holding_mode", "holding_mode "+bad)
	}
}

// 立花 cannot execute 一日信用, and its force-flatten depends entirely on
// holding_mode being correct.
func TestPromote_RejectsMarginOneday(t *testing.T) {
	y := strings.Replace(bnfPaperYAML, "exec_kind: cash", "exec_kind: margin_oneday", 1)
	mustReject(t, &Promoter{HardLimits: testHardLimits()}, []byte(y), "margin_oneday", "margin_oneday")
}

func TestPromote_RejectsZeroQuantity(t *testing.T) {
	y := strings.Replace(bnfPaperYAML, "quantity: 100", "quantity: 0", 1)
	mustReject(t, &Promoter{HardLimits: testHardLimits()}, []byte(y), "quantity must be > 0", "zero quantity")
}

func TestPromote_AllowsNoTrade(t *testing.T) {
	y := strings.Replace(bnfPaperYAML, "strategy_name: bnf_reversion", "strategy_name: no_trade", 1)
	if _, err := (&Promoter{HardLimits: testHardLimits()}).Promote([]byte(y)); err != nil {
		t.Fatalf("no_trade must be allowed, got %v", err)
	}
}

// 拘束が無いと「52週高値の候補は常に abs_momentum の候補でもある」ため LLM が
// 毎回そちらを選び、包含関係にある戦略の標本が永久にゼロになる(実測 285 run 中
// high_52w_momentum 選択 0 回)。
func TestPromote_RejectsSlotStrategyMismatch(t *testing.T) {
	p := &Promoter{HardLimits: testHardLimits(), ExpectedStrategy: config.StrategyHigh52wMomentum}
	mustReject(t, p, []byte(bnfPaperYAML), "slot", "slot-bound promote with a different strategy")
}

// 拘束は「他戦略に枠を横取りされない」ためで entry の強制ではないので、no_trade は例外。
func TestPromote_SlotStrategyAllowsNoTrade(t *testing.T) {
	y := strings.Replace(bnfPaperYAML, "strategy_name: bnf_reversion", "strategy_name: no_trade", 1)
	p := &Promoter{HardLimits: testHardLimits(), ExpectedStrategy: config.StrategyHigh52wMomentum}
	if _, err := p.Promote([]byte(y)); err != nil {
		t.Fatalf("no_trade must be allowed under a slot binding, got %v", err)
	}
}

func TestPromote_SlotStrategyMatchAccepted(t *testing.T) {
	p := &Promoter{HardLimits: testHardLimits(), ExpectedSymbol: "7203", ExpectedStrategy: config.StrategyBNFReversion}
	cfg, err := p.Promote([]byte(bnfPaperYAML))
	if err != nil {
		t.Fatalf("matching slot strategy must pass, got %v", err)
	}
	if cfg.StrategyName != config.StrategyBNFReversion {
		t.Fatalf("bad cfg: %+v", cfg)
	}
}

// The candidate menu must never become a live path.
func TestPromote_NoMenuStrategyCanReachLive(t *testing.T) {
	hl := testHardLimits() // LiveAllowedStrategies is empty, as committed
	for _, name := range AdvisorCandidateStrategies {
		y := strings.Replace(bnfPaperYAML, "strategy_name: bnf_reversion", "strategy_name: "+string(name), 1)
		live := strings.Replace(y, "mode: paper_config", "mode: live_config", 1)
		if _, err := (&Promoter{HardLimits: hl}).Promote([]byte(live)); err == nil {
			t.Fatalf("%s: a live_config from the advisor must be rejected", name)
		}
		if hl.AllowsLiveStrategy(name) {
			t.Fatalf("%s is on the live allowlist; the paper menu must not overlap it", name)
		}
	}
}
