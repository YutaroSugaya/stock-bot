package command

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

// AdvisorCandidateStrategies is the PAPER menu the advisor may propose from. It
// is DISTINCT from hard_limits.live_allowed_strategies (the LIVE gate): widening
// it opens no live path, because Promote forces mode=paper_config.
// 正本は戦略カタログ(`strategy.Catalog` の Menu 列)。ここに名前を写さない。
var AdvisorCandidateStrategies = strategy.MenuNames()

// Promoter validates one advisor YAML into an arm-ready StrategyConfig,
// fail-closed: ANY violation returns an error and the caller arms nothing.
type Promoter struct {
	HardLimits     *config.HardLimits
	ExpectedSymbol string                // "" = accept the config's own symbol (still hard-limit checked)
	Menu           []config.StrategyName // nil = AdvisorCandidateStrategies
	// ExpectedStrategy binds this promotion to the strategy the round-robin gave
	// the slot to ("" = any menu strategy). Without it a strategy whose gate is a
	// subset of another's (52週高値 ⊂ abs_momentum) is out-chosen by the LLM every
	// round and collects zero forward samples — 実測 285 run 中 0 回。
	// no_trade is always allowed: the binding stops slot poaching, it never forces
	// an entry.
	ExpectedStrategy config.StrategyName
}

// Promote は LLM が返した YAML を検証する(LLM 経路の入口)。
// 決定論 arm の運用経路は PromoteConfig(決定論テンプレート)を通る。
func (p *Promoter) Promote(parsedYAML []byte) (*config.StrategyConfig, error) {
	var c config.StrategyConfig
	if err := yaml.Unmarshal(parsedYAML, &c); err != nil {
		return nil, fmt.Errorf("promote: parse yaml: %w", err)
	}
	return p.PromoteConfig(&c)
}

// PromoteConfig は組み立て済みの config を検証する。**LLM が無くなっても検証は残す**:
// 「メニュー外の戦略」「live_config 混入」「holding_mode 空」
// 「margin_oneday」のような事故は、決定論のテンプレートでも**コードのバグとして**
// 起こりうる。Promoter の役割が「LLM 出力の検査」から「arm 直前の最終検査」に変わる。
func (p *Promoter) PromoteConfig(in *config.StrategyConfig) (*config.StrategyConfig, error) {
	if in == nil {
		return nil, fmt.Errorf("promote: nil config")
	}
	c := *in
	if c.StrategyName == "" {
		c.StrategyName = config.StrategyNoTrade
	}
	// Forcing paper HERE is what makes menu widening safe — the advisor never arms live.
	if c.Mode != config.ModePaper {
		return nil, fmt.Errorf("promote: mode %q rejected — advisor is paper_config only", c.Mode)
	}
	if p.ExpectedSymbol != "" && c.Symbol != p.ExpectedSymbol {
		return nil, fmt.Errorf("promote: symbol mismatch: got %q want %q", c.Symbol, p.ExpectedSymbol)
	}
	if !p.inMenu(c.StrategyName) {
		return nil, fmt.Errorf("promote: strategy %q not in advisor candidate menu", c.StrategyName)
	}
	if p.ExpectedStrategy != "" && c.StrategyName != config.StrategyNoTrade && c.StrategyName != p.ExpectedStrategy {
		return nil, fmt.Errorf("promote: strategy %q rejected — this slot is bound to %q (per-strategy round-robin; no_trade は可)",
			c.StrategyName, p.ExpectedStrategy)
	}
	if err := c.ValidateAgainstHardLimits(p.HardLimits); err != nil {
		return nil, fmt.Errorf("promote: %w", err)
	}
	// An empty holding_mode is NOT harmless: it flows to Signal.HoldingMode, makes
	// execKindFor("") fall through to margin_oneday (一日信用), and ForceFlatten then
	// skips the position because it is not HoldingIntraday — 引け前フラット化 broken
	// through DATA rather than control flow. The prompt is soft; this is the gate.
	if !c.HoldingMode.Valid() {
		return nil, fmt.Errorf("promote: holding_mode %q invalid (must be intraday|multiday) — an empty value defeats 引け前フラット化", c.HoldingMode)
	}
	// 立花 does not support 一日信用, and it is the exec kind whose force-flatten
	// depends entirely on holding_mode being right.
	if c.ExecKind == order.ExecMarginOneday {
		return nil, fmt.Errorf("promote: exec_kind margin_oneday not allowed from the advisor (立花 非対応・引け前フラット化 依存)")
	}
	if c.ExecKind != "" && c.ExecKind != order.ExecCash && c.ExecKind != order.ExecMarginSystem {
		return nil, fmt.Errorf("promote: exec_kind %q invalid (must be cash|margin_system)", c.ExecKind)
	}
	if c.StrategyName != config.StrategyNoTrade {
		if c.Risk.Quantity <= 0 {
			return nil, fmt.Errorf("promote: quantity must be > 0 for trading strategy %q", c.StrategyName)
		}
		// 向きは **戦略ごとに事前登録された値**(ArmDirection)か buy_only だけを通す。
		//
		// 契約が変わった: 以前はここが「advisor はロング only」の
		// hard gate で、`buy_only` 以外を全部 reject していた。4 戦略(abs / atr /
		// donchian の v2 + high_52w)は入口の式が既に両向きで書かれており、
		// `direction: both` を開けることで空売り標本を取る。**ゲートを外すのではなく、
		// 事前登録の表と一致することを要求する**形にした — テンプレートのバグで
		// long-only 戦略(BNF。鏡像は台帳 #18 で reject 済み)に売りが入るのは事故。
		//
		// 🛑 `buy_only` は常に通す: 事前登録が both の戦略でも、買いに絞るのは
		// 空売りを**開かない**方向の縮退なので安全側(LLM 経路の契約もこれ)。
		if c.Entry.Direction != config.DirectionBuyOnly && c.Entry.Direction != ArmDirection(c.StrategyName) {
			return nil, fmt.Errorf("promote: direction %q rejected — %q の事前登録は %q(または buy_only)",
				c.Entry.Direction, c.StrategyName, ArmDirection(c.StrategyName))
		}
	}
	return &c, nil
}

// 🗑 **`usesConfigExit` の SL 必須ゲートは撤去した。**
//
// あれは「プロンプトがまだ tp/sl を返す契約なので、値が欠けた config を『LLM が指示を
// 無視した』シグナルとして弾く」ためのものだった。ATR 化以降、**6戦略の
// 実際の出口は cfg.Exit ではなく ATR 倍数**で、円幅は 1 度も読まれていない
// (ATR が取れない銘柄は `no_trade("no_atr")` に落ちる — 黙って円幅へ縮退する経路は無い)。
// LLM が arm 経路から外れて「指示を無視したか」を測る対象自体が消えたので、
// 残しておくと**決定論テンプレート(exit 全て 0)が 6 戦略で全滅する**だけになる。
//
// コード内コメントが要求していた 3 点同時更新はこのコミットで満たしている:
// ゲート撤去 + guard test(advisor_promote_test)+ prompts/generate_strategy_config.md。

func (p *Promoter) inMenu(name config.StrategyName) bool {
	if name == config.StrategyNoTrade {
		return true
	}
	menu := p.Menu
	if menu == nil {
		menu = AdvisorCandidateStrategies
	}
	for _, m := range menu {
		if m == name {
			return true
		}
	}
	return false
}
