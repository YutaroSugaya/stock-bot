package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// StrategyConfig is the live-mutable per-symbol strategy parameter set. It is
// snapshotted onto a Position at entry and never mutated thereafter (config freeze).
type StrategyConfig struct {
	ConfigID     string       `yaml:"config_id"`
	Symbol       string       `yaml:"symbol"`
	StrategyName StrategyName `yaml:"strategy_name"`
	Mode         Mode         `yaml:"mode"`
	HoldingMode  HoldingMode  `yaml:"holding_mode"`
	ExecKind     ExecKind     `yaml:"exec_kind"`
	TTLMinutes   int          `yaml:"ttl_minutes"`

	// MarketRegime is the advisor's stated rationale (optional). Metadata only — no
	// safety/hard-limit check reads it; it exists so 「なぜその判断をしたか」 survives
	// into the persisted raw_yaml + logs.
	MarketRegime struct {
		Type       string  `yaml:"type,omitempty"`
		Confidence float64 `yaml:"confidence,omitempty"`
		Reason     string  `yaml:"reason,omitempty"`
	} `yaml:"market_regime,omitempty"`

	Entry struct {
		Direction      Direction `yaml:"direction"`
		MaxSpreadTicks float64   `yaml:"max_spread_ticks"`
	} `yaml:"entry"`

	Exit struct {
		// 出口幾何は円/株(建値からの距離)。呼値は銘柄と価格帯で 0.1〜10円に変わるので、
		// tick 建てだと同じ tick 数が銘柄ごとに別の賭けになる。
		TakeProfitJPY  float64 `yaml:"take_profit_jpy"`
		StopLossJPY    float64 `yaml:"stop_loss_jpy"`
		MaxHoldMinutes int     `yaml:"max_hold_minutes"`

		ExtensionMaxMinutes    int     `yaml:"extension_max_minutes"`
		ExtensionUnrealizedJPY float64 `yaml:"extension_unrealized_jpy"`
		EarlyExitWindowMinutes int     `yaml:"early_exit_window_minutes"`
		EarlyExitTargetJPY     float64 `yaml:"early_exit_target_jpy"`
		RatchetArmJPY          float64 `yaml:"ratchet_arm_jpy"`
		RatchetGivebackJPY     float64 `yaml:"ratchet_giveback_jpy"`
	} `yaml:"exit"`

	Risk struct {
		Quantity               int `yaml:"quantity"`
		MaxOpenPositions       int `yaml:"max_open_positions"`
		MaxTradesInThisWindow  int `yaml:"max_trades_in_this_window"`
		MaxLossInThisWindowJPY int `yaml:"max_loss_in_this_window_jpy"`
	} `yaml:"risk"`

	// Tuning is an optional strategy-specific parameter map for backtest robustness
	// sweeps. It never affects safety/hard-limit validation; absent keys fall back to
	// the strategy's pre-registered constants.
	Tuning map[string]float64 `yaml:"tuning,omitempty"`

	// ActivatedAt zero means "no TTL tracking" (never expires on time).
	ActivatedAt time.Time `yaml:"-"`

	// AdvisorRunID is the advisor_runs.run_id that produced this config. Deliberately
	// `yaml:"-"`: the Promoter parses LLM-returned YAML, so the LLM must not be able to
	// forge its own provenance. Empty for human/test/sentinel configs.
	AdvisorRunID string `yaml:"-"`
}

// YAMLText renders the config back to YAML for the strategy_configs.raw_yaml audit
// column(「その時どの config で動いていたか」を全文で残す)。observability 用途なので、
// marshal 失敗時は空文字で縮退する(取引は止めない)。
func (c *StrategyConfig) YAMLText() string {
	if c == nil {
		return ""
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// Fingerprint は取引に効く中身の短いハッシュ。**config_id を内容に縛るため**にある。
//
// config 凍結は「同じ config_id は永久に同じ内容」で成り立っている(strategy_configs の
// upsert は既存 raw_yaml を上書きしない)。だから内容を変えたまま id を使い回すと、
// 台帳に残るのは初版で、実際に発注した設定と食い違う — トレードを説明できなくなる。
// live の config で実際に起きうる(exec_kind を
// margin_general → margin_system → cash → margin_system と変えても id が同じで、
// DB には初版の margin_general が凍ったまま残っていた)。
//
// **銘柄と config_id 自体、および記録用の付帯情報は指紋に含めない** — 1 つの
// テンプレを 200 銘柄に複製したときは同じ設定なので同じ指紋であってほしい。
func (c *StrategyConfig) Fingerprint() string {
	if c == nil {
		return ""
	}
	b := *c
	b.ConfigID, b.Symbol, b.AdvisorRunID, b.ActivatedAt = "", "", "", time.Time{}
	sum := sha256.Sum256([]byte(b.YAMLText()))
	return hex.EncodeToString(sum[:4])
}

// IsExpired reports whether the config has aged past its TTL relative to now.
func (c *StrategyConfig) IsExpired(now time.Time) bool {
	if c == nil || c.TTLMinutes <= 0 || c.ActivatedAt.IsZero() {
		return false
	}
	return now.After(c.ActivatedAt.Add(time.Duration(c.TTLMinutes) * time.Minute))
}

// LoadStrategyConfig reads and parses the active strategy config YAML.
func LoadStrategyConfig(path string) (*StrategyConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read strategy_config %q: %w", path, err)
	}
	var c StrategyConfig
	if err := decodeStrict(b, &c); err != nil {
		return nil, fmt.Errorf("parse strategy_config %q: %w", path, err)
	}
	if c.StrategyName == "" {
		c.StrategyName = StrategyNoTrade
	}
	return &c, nil
}

// ValidateAgainstHardLimits fails closed if any field steps outside the immutable
// hard-limit ranges. Returns the first violation.
func (c *StrategyConfig) ValidateAgainstHardLimits(hl *HardLimits) error {
	if c == nil {
		return fmt.Errorf("nil strategy config")
	}
	if hl == nil {
		return fmt.Errorf("nil hard limits")
	}
	if !hl.AllowsSymbol(c.Symbol) {
		return fmt.Errorf("symbol %q not in allowed_symbols whitelist", c.Symbol)
	}
	if c.Risk.Quantity != 0 && !hl.Quantity.Contains(c.Risk.Quantity) {
		return fmt.Errorf("quantity %d outside hard limit %+v", c.Risk.Quantity, hl.Quantity)
	}
	// ここは価格を知らない層なので絶対値の sanity のみ。「建値の何%まで」は entry 時に
	// risk.EvaluateOrderBoundary が判定する — 同じ 500 円でも 700 円株と 76,000 円株では
	// 意味が違うため。
	if c.Exit.TakeProfitJPY != 0 && !hl.TakeProfitJPY.Contains(c.Exit.TakeProfitJPY) {
		return fmt.Errorf("take_profit_jpy %.1f outside %+v", c.Exit.TakeProfitJPY, hl.TakeProfitJPY)
	}
	if c.Exit.StopLossJPY != 0 && !hl.StopLossJPY.Contains(c.Exit.StopLossJPY) {
		return fmt.Errorf("stop_loss_jpy %.1f outside %+v", c.Exit.StopLossJPY, hl.StopLossJPY)
	}
	if c.Exit.MaxHoldMinutes != 0 && !hl.MaxHoldMinutes.Contains(c.Exit.MaxHoldMinutes) {
		return fmt.Errorf("max_hold_minutes %d outside %+v", c.Exit.MaxHoldMinutes, hl.MaxHoldMinutes)
	}
	// 以下 4 本は hard_limits.yaml に range を書きながら**ここで束縛していなかった**。
	// advisor が生成した config はこの関数しか通らないので、
	// 「hard limit に書けば効く」という前提が成り立っていなかった。
	// Max==0 = その range を宣言していない → 縛らない(未宣言を「全部拒否」にすると
	// range を 1 本足すたびに既存 yaml が起動不能になる)。宣言漏れの側は
	// catastrophe_guards_test が committed yaml に対して塞ぐ。
	if hl.MaxTradesInThisWindow.Max != 0 && c.Risk.MaxTradesInThisWindow != 0 && !hl.MaxTradesInThisWindow.Contains(c.Risk.MaxTradesInThisWindow) {
		return fmt.Errorf("max_trades_in_this_window %d outside %+v", c.Risk.MaxTradesInThisWindow, hl.MaxTradesInThisWindow)
	}
	if hl.MaxLossInWindowJPY.Max != 0 && c.Risk.MaxLossInThisWindowJPY != 0 && !hl.MaxLossInWindowJPY.Contains(c.Risk.MaxLossInThisWindowJPY) {
		return fmt.Errorf("max_loss_in_this_window_jpy %d outside %+v", c.Risk.MaxLossInThisWindowJPY, hl.MaxLossInWindowJPY)
	}
	// 上限を緩められると risk gate の板スプレッド判定が実質 no-op になる。
	if hl.MaxSpreadTicks.Max != 0 && c.Entry.MaxSpreadTicks != 0 && !hl.MaxSpreadTicks.Contains(c.Entry.MaxSpreadTicks) {
		return fmt.Errorf("max_spread_ticks %.1f outside %+v", c.Entry.MaxSpreadTicks, hl.MaxSpreadTicks)
	}
	if hl.ConfigTTLMinutes.Max != 0 && c.TTLMinutes != 0 && !hl.ConfigTTLMinutes.Contains(c.TTLMinutes) {
		return fmt.Errorf("ttl_minutes %d outside %+v", c.TTLMinutes, hl.ConfigTTLMinutes)
	}
	return nil
}

// ForSymbol はその銘柄に当てる config。"*" のテンプレートは銘柄ごとに複製し、config_id に銘柄を
// 足す(Postgres の FK で一意にする)。銘柄固定の config は一致するときだけ自身を返す。
func (c *StrategyConfig) ForSymbol(sym string) *StrategyConfig {
	if c == nil {
		return nil
	}
	switch c.Symbol {
	case sym:
		return c
	case "*":
		out := *c // shallow copy (nested Entry/Exit/Risk are value types)
		out.Symbol = sym
		out.ConfigID = c.ConfigID + "_" + sym
		if c.Tuning != nil { // deep-copy the map so per-symbol clones never alias the template
			out.Tuning = make(map[string]float64, len(c.Tuning))
			for k, v := range c.Tuning {
				out.Tuning[k] = v
			}
		}
		return &out
	default:
		return nil
	}
}
