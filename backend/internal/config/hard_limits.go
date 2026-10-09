package config

import (
	"fmt"
	"os"
)

// HardLimits is the immutable safety contract loaded once at startup. Every
// live-mutable config value is range-checked against it; symbols fail closed if not
// whitelisted.
type HardLimits struct {
	AllowedSymbols []string `yaml:"allowed_symbols"`

	// LiveAllowedStrategies is the strategy allowlist for mode live_config (no_trade
	// always passes). Adding a name here is the HUMAN promotion commit after edge
	// proof; without it an unproven strategy could reach live via strategy_config alone.
	LiveAllowedStrategies []string `yaml:"live_allowed_strategies"`

	// LoanableSymbols は**制度信用の売建が出せる銘柄**(JPX の貸借銘柄一覧由来・人間 commit)。
	//
	// 🛑 **fail-close**: 空なら売りシグナルを**全 reject**する(買いは従来どおり)。
	// 静的な推測に縮退しない — `allowed_symbols` と同じ作法。一覧を作るのは人間の作業
	// (JPX の貸借銘柄一覧を取得 → 4 文字コードへ整形 → commit)で、**用意されるまで
	// 売り側は標本ゼロ**。それが「エラーも出さずにゼロ」に見えないよう、発注前ゲート
	// (`risk.EvaluateShortLoanable`)が理由を signal_rejections に残す。
	//
	// 🛑 選定規則はサイクル中に動かさない(`allowed_symbols` と同じ扱い)。
	LoanableSymbols []string `yaml:"loanable_symbols"`
	// LoanableSymbolsAsOf は貸借一覧の生成日(YYYY-MM-DD)。bot は読まない — 読むのは
	// 朝ルーチンの expiry-manifest(45 / 60 日で警告)と loanable_guard_test の正規表現。
	// フィールドとして持つのは、未知キーを拒否する decoder を通すため。
	LoanableSymbolsAsOf string `yaml:"loanable_symbols_as_of"`

	Quantity IntRange `yaml:"quantity"`
	// 出口幅は **円/株**。ここは価格を知らない層なので絶対値の sanity レンジ。
	// 「建値の何%まで」の経済的な上限は order_boundary の % を entry 時に見る。
	TakeProfitJPY         FloatRange `yaml:"take_profit_jpy"`
	StopLossJPY           FloatRange `yaml:"stop_loss_jpy"`
	MaxHoldMinutes        IntRange   `yaml:"max_hold_minutes"`
	MaxTradesInThisWindow IntRange   `yaml:"max_trades_in_this_window"`
	MaxLossInWindowJPY    IntRange   `yaml:"max_loss_in_this_window_jpy"`
	MaxSpreadTicks        FloatRange `yaml:"max_spread_ticks"`
	ConfigTTLMinutes      IntRange   `yaml:"config_ttl_minutes"`

	OrderBoundary struct {
		MaxLossPerTradeJPY int `yaml:"max_loss_per_trade_jpy"`
		// 建値に対する % の上限(entry 時に建値を使って判定)。tick 数や円の絶対値
		// では価格帯ごとに意味が変わってしまうため、経済的な上限はここで % で持つ。
		MaxStopLossPct   float64 `yaml:"max_stop_loss_pct"`
		MaxTakeProfitPct float64 `yaml:"max_take_profit_pct"`
	} `yaml:"order_boundary"`

	Margin struct {
		MinCollateralJPY int     `yaml:"min_collateral_jpy"`
		RequiredRate     float64 `yaml:"required_rate"`
		MaintenanceRatio float64 `yaml:"maintenance_ratio"`

		// 信用建玉の資金コスト(年率・%表記。2.5 = 年2.50%)。paper でも live でも
		// 同じ料率で trade 行の carry_jpy を作る — 紙の net が実弾より甘く出ると
		// forward のエッジ判定が嘘になる。未設定(0)なら carry は 0 のままで、
		// レートは決して捏造しない。
		BuyAnnualRatePct       float64 `yaml:"buy_annual_rate_pct"`      // 買方金利
		SellLendingAnnualPct   float64 `yaml:"sell_lending_annual_pct"`  // 貸株料(売り)
		SettlementBusinessDays int     `yaml:"settlement_business_days"` // 受渡(日本株 T+2)
	} `yaml:"margin"`

	SessionHours SessionHours `yaml:"session_hours"`

	Paper struct {
		SimulatedSlippageTicks float64 `yaml:"simulated_slippage_ticks"`
		APIFeeJPYPerTrade      int     `yaml:"api_fee_jpy_per_trade"`
		// BalanceJPY は paper 口座の残高(0 = 既定100万円)。研究モード(全トリガー
		// 採用・同時多数ポジ)で collateral ゲートがサンプルを censoring しないよう
		// 大きく設定できる。実弾の建付けとは無関係(paper broker のみが読む)。
		BalanceJPY int `yaml:"balance_jpy"`
	} `yaml:"paper"`

	Cooldown struct {
		AfterLossSeconds       int `yaml:"after_loss_seconds"`
		AfterTakeProfitSeconds int `yaml:"after_take_profit_seconds"`
	} `yaml:"cooldown"`
}

// AllowsSymbol reports whether sym is on the whitelist (fail-close).
func (h *HardLimits) AllowsSymbol(sym string) bool {
	for _, s := range h.AllowedSymbols {
		if s == sym {
			return true
		}
	}
	return false
}

// AllowsShortSymbol は「この銘柄で制度信用の売建を出せるか」(fail-close)。
// 一覧が空なら常に false = 売り側は 1 本も建たない。
func (h *HardLimits) AllowsShortSymbol(sym string) bool {
	if h == nil {
		return false
	}
	for _, s := range h.LoanableSymbols {
		if s == sym {
			return true
		}
	}
	return false
}

// AllowsLiveStrategy reports whether a strategy may run under mode live_config
// (fail-close). no_trade always passes; everything else needs an explicit
// human-committed entry in live_allowed_strategies.
func (h *HardLimits) AllowsLiveStrategy(name StrategyName) bool {
	if name == "" || name == StrategyNoTrade {
		return true
	}
	for _, s := range h.LiveAllowedStrategies {
		if StrategyName(s) == name {
			return true
		}
	}
	return false
}

// LoadHardLimits reads, parses and structurally validates hard_limits.yaml.
func LoadHardLimits(path string) (*HardLimits, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read hard_limits %q: %w", path, err)
	}
	var h HardLimits
	if err := decodeStrict(b, &h); err != nil {
		return nil, fmt.Errorf("parse hard_limits %q: %w", path, err)
	}
	if err := h.validate(); err != nil {
		return nil, fmt.Errorf("invalid hard_limits %q: %w", path, err)
	}
	return &h, nil
}

// validate checks the contract is internally coherent.
func (h *HardLimits) validate() error {
	if len(h.AllowedSymbols) == 0 {
		return fmt.Errorf("allowed_symbols must list at least one symbol (fail-close)")
	}
	ranges := []struct {
		name     string
		min, max float64
	}{
		{"quantity", float64(h.Quantity.Min), float64(h.Quantity.Max)},
		{"take_profit_jpy", h.TakeProfitJPY.Min, h.TakeProfitJPY.Max},
		{"stop_loss_jpy", h.StopLossJPY.Min, h.StopLossJPY.Max},
		{"max_hold_minutes", float64(h.MaxHoldMinutes.Min), float64(h.MaxHoldMinutes.Max)},
		{"max_trades_in_this_window", float64(h.MaxTradesInThisWindow.Min), float64(h.MaxTradesInThisWindow.Max)},
		{"max_loss_in_this_window_jpy", float64(h.MaxLossInWindowJPY.Min), float64(h.MaxLossInWindowJPY.Max)},
		{"max_spread_ticks", h.MaxSpreadTicks.Min, h.MaxSpreadTicks.Max},
		{"config_ttl_minutes", float64(h.ConfigTTLMinutes.Min), float64(h.ConfigTTLMinutes.Max)},
	}
	for _, r := range ranges {
		if r.min > r.max {
			return fmt.Errorf("%s range inverted: min %g > max %g", r.name, r.min, r.max)
		}
		// max 未宣言(=0)を許すと ValidateAgainstHardLimits がその項目を
		// 「縛らない」と解釈し、hard limit に見えて無制限になる。読み込み時に落とす。
		if r.max <= 0 {
			return fmt.Errorf("%s range が未宣言(max=%g)— hard limit として効かないので fail-close", r.name, r.max)
		}
	}
	if h.Margin.RequiredRate < 0 || h.Margin.RequiredRate > 1 {
		return fmt.Errorf("margin.required_rate %.2f must be in [0,1]", h.Margin.RequiredRate)
	}
	if h.Margin.MaintenanceRatio < 0 || h.Margin.MaintenanceRatio > 1 {
		return fmt.Errorf("margin.maintenance_ratio %.2f must be in [0,1]", h.Margin.MaintenanceRatio)
	}
	if len(h.SessionHours.Sessions) == 0 {
		return fmt.Errorf("session_hours.sessions must list at least one window")
	}
	return nil
}
