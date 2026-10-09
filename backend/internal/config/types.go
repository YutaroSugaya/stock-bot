// Package config loads and validates the two-tier configuration: immutable
// hard_limits (safety contract, validated once at startup) and live-mutable
// bot_config + strategy_config.
package config

import "stockbot/backend/internal/domain/order"

type (
	HoldingMode = order.HoldingMode
	ExecKind    = order.ExecKind
)

const (
	HoldingIntraday = order.HoldingIntraday
	HoldingMultiday = order.HoldingMultiday

	ExecCash          = order.ExecCash
	ExecMarginOneday  = order.ExecMarginOneday
	ExecMarginGeneral = order.ExecMarginGeneral
	ExecMarginSystem  = order.ExecMarginSystem
)

// StrategyName identifies a named strategy for engine dispatch.
type StrategyName string

const (
	StrategyNoTrade            StrategyName = "no_trade"
	StrategyTimeSeriesMomentum StrategyName = "time_series_momentum"

	// Unproven candidate templates: backtest screen only until net-of-cost edge is shown.
	StrategyMACross          StrategyName = "ma_cross"
	StrategyAbsMomentum      StrategyName = "abs_momentum"
	StrategyHigh52wMomentum  StrategyName = "high_52w_momentum"
	StrategyDonchianBreakout StrategyName = "donchian_breakout"
	StrategyATRBreakout      StrategyName = "atr_breakout"
	// v2 は v1 と入口だけが違い、出口は同一(TP=3.0×ATR / SL=2.0×ATR)。forward-report
	// -strategy の分計単位なので、差分を入口に帰属させるには v1 と別名でなければならない。
	StrategyAbsMomentumV2      StrategyName = "abs_momentum_v2"
	StrategyATRBreakoutV2      StrategyName = "atr_breakout_v2"
	StrategyDonchianBreakoutV2 StrategyName = "donchian_breakout_v2"

	// トレンド系4戦略の **入口が完全に同一で出口だけが違う兄弟アーム**。
	// v3(置き換え)にはしない — 置き換えると「固定 TP と トレール のどちらが良いか」
	// という問い自体が測れなくなる。名前はベース名 + `_trail`(`bnf_reversion_trail`
	// と同じ規則)で、`command.ArmDirection` / `strategy.MaxHoldBusinessDays` は
	// このサフィックスを剥がして基のアームの属性を引く(片方だけ直す事故の防止)。
	StrategyAbsMomentumV2Trail      StrategyName = "abs_momentum_v2_trail"
	StrategyATRBreakoutV2Trail      StrategyName = "atr_breakout_v2_trail"
	StrategyDonchianBreakoutV2Trail StrategyName = "donchian_breakout_v2_trail"
	StrategyHigh52wMomentumTrail    StrategyName = "high_52w_momentum_trail"
	StrategyRSI2Reversion           StrategyName = "rsi2_reversion"
	StrategyBollingerReversion      StrategyName = "bollinger_reversion"
	StrategyGapReversion            StrategyName = "gap_reversion"
	StrategyHighVolumePremium       StrategyName = "high_volume_premium" // GKM attention premium
	StrategyPostJumpDrift           StrategyName = "post_jump_drift"     // EAR-proxy PEAD (information under-reaction)

	// BNF panic-reversion A/B (capped reversion target vs uncapped ATR ratchet)
	// + the euphoria-short mirror (symmetry probe).
	StrategyBNFReversion      StrategyName = "bnf_reversion"
	StrategyBNFReversionTrail StrategyName = "bnf_reversion_trail"
	StrategyBNFEuphoriaShort  StrategyName = "bnf_euphoria_short"

	// bnf ファミリーの新入口 2 つ。
	// 閾値は bnf から借りるだけで新しく選ばない。兄弟は base + `_trail`(EntryArmOf が畳む)。
	//   day2      … 判定時点を前日終値から場中へ移す(当日 −12% を割った瞬間に建てる)
	//   stabilized … bnf に「現在値 < 前日終値なら見送る」ゲートを 1 つ足す(下げ止まり確認)
	StrategyBNFDay2Reversion            StrategyName = "bnf_day2_reversion"
	StrategyBNFDay2ReversionTrail       StrategyName = "bnf_day2_reversion_trail"
	StrategyBNFStabilizedReversion      StrategyName = "bnf_stabilized_reversion"
	StrategyBNFStabilizedReversionTrail StrategyName = "bnf_stabilized_reversion_trail"

	// Previous-day BNF panic + same-day 5-minute reversal confirmation.
	// UNPROVEN. forward paper で測る(バックテストは使わない)。
	// 兄弟は base + `_trail`(TP を同じ距離の ratchet に置き換える)。
	StrategyBNFIntradayReversion      StrategyName = "bnf_intraday_reversion"
	StrategyBNFIntradayReversionTrail StrategyName = "bnf_intraday_reversion_trail"
)

// Direction constrains which sides a strategy/config may trade.
type Direction string

const (
	DirectionNone     Direction = ""
	DirectionBoth     Direction = "both"
	DirectionBuyOnly  Direction = "buy_only"
	DirectionSellOnly Direction = "sell_only"
)

// Mode is the bot operating mode (fail-safe paper default).
type Mode string

const (
	ModePaper    Mode = "paper_config"
	ModeLive     Mode = "live_config"
	ModeDisabled Mode = "disabled"
)

// Valid reports whether m is a recognised mode. A typo'd mode must be rejected at
// startup: treating it as non-live bypasses the live-only gates (allowlist, reconcile)
// while a real broker could still place orders.
func (m Mode) Valid() bool {
	switch m {
	case ModePaper, ModeLive, ModeDisabled:
		return true
	default:
		return false
	}
}

// BrokerKind selects the broker adapter at wiring time.
type BrokerKind string

const (
	BrokerPaper     BrokerKind = "paper"
	BrokerTachibana BrokerKind = "tachibana"
	// BrokerPaperLiveFeed は本番フィード(read-only)+ paper 執行。発注系は paper に
	// 閉じるので実弾は飛ばないが、価格・日足は実市場のものを使う。
	BrokerPaperLiveFeed BrokerKind = "paper_live_feed"
)

// SupportsExecKind reports whether the broker adapter can actually execute the given
// 執行区分。誤った区分で流すと注文が拒否されるか、Position が誤った ExecKind で
// 凍結されるので、ValidateBrokerCapabilities が起動時に落とす。
//
// 🛑 立花 e支店で通るのは **現物と制度信用(6ヶ月)**。
//   - 一日信用: `sGenkinShinyouKubun` に区分が無い(API 非対応)
//   - 一般信用(6ヶ月): 区分 "6" は仕様に**存在する**が、**口座で拒否された**
//     (4751 で「現金信用区分に誤りがあります」)。公式サンプルの信用注文6例も
//     全て制度信用 "2"。口座の取扱いを確認できたら再度開ける。
//
// CLAUDE.md の旧記述「現物+一般信用のみ」は誤りだったので同時に訂正した。
func (k BrokerKind) SupportsExecKind(e ExecKind) bool {
	// paper_live_feed は紙執行だが、検証結果をそのまま立花へ移せるよう制約を揃える。
	if k == BrokerTachibana || k == BrokerPaperLiveFeed {
		return e == ExecCash || e == ExecMarginSystem
	}
	return true
}

// ServesKlines reports whether the adapter can serve 日足 from a real market API
// (paper 単体は起動時 CSV シードのみ)。日足リフレッシュの可否を mode で判定すると
// paper_live_feed の日足が起動時のまま凍結し、day-horizon 戦略が stale で永久スキップになる。
func (k BrokerKind) ServesKlines() bool {
	return k == BrokerTachibana || k == BrokerPaperLiveFeed
}

// IntRange is an inclusive integer range used for hard-limit validation.
type IntRange struct {
	Min int `yaml:"min"`
	Max int `yaml:"max"`
}

// Contains reports whether v is within [Min, Max].
func (r IntRange) Contains(v int) bool { return v >= r.Min && v <= r.Max }

// FloatRange is an inclusive float range used for hard-limit validation.
type FloatRange struct {
	Min float64 `yaml:"min"`
	Max float64 `yaml:"max"`
}

// Contains reports whether v is within [Min, Max].
func (r FloatRange) Contains(v float64) bool { return v >= r.Min && v <= r.Max }
