// Package strategy は純粋な売買ルール層。Evaluate は I/O・DB・rand を持たず、
// 実行可否は risk gate と order manager が決める。
package strategy

import (
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/ta"
)

type Decision string

const (
	DecisionEnter   Decision = "ENTER"
	DecisionNone    Decision = "NONE"
	DecisionNoTrade Decision = "NO_TRADE"
)

// 出口幾何は建玉時に Position へ凍結される **円/株** のスナップショット。呼値(tick)は発注価格を
// グリッドに丸めるときだけ使う — 出口幾何を tick 建てにすると、呼値が銘柄と価格帯で 0.1〜10円に
// 変わるせいで同じ config が銘柄ごとに別の賭けになる。
type Signal struct {
	Decision   Decision
	SignalID   string
	Symbol     string
	Side       order.Side
	EntryPrice float64

	TakeProfitJPY  float64 // 建値からの利確幅(円/株)。0 = 無し(trail 等)
	StopLossJPY    float64 // 建値からの損切幅(円/株)
	MaxHoldMinutes int
	Quantity       int

	ExtensionMaxMinutes    int
	ExtensionUnrealizedJPY float64
	EarlyExitWindowMinutes int
	EarlyExitTargetJPY     float64
	RatchetArmJPY          float64
	RatchetGivebackJPY     float64

	// 同 symbol 同 side の同時建玉上限。0/1 = ナンピン禁止、>1 = advisor v2 のピラミッディング。
	MaxConcurrent int

	HoldingMode  order.HoldingMode
	Reason       string
	ConfigID     string
	StrategyName config.StrategyName
	CreatedAt    time.Time
}

func (s Signal) IsEntry() bool {
	return s.Decision == DecisionEnter && s.Side.Valid() && s.Quantity > 0
}

// active config の Exit/Risk スナップショットをそのまま出口にする ENTER Signal。
func configExitSignal(in EvalInput, side order.Side, entry float64, name config.StrategyName, reason string) Signal {
	cfg := in.Config
	return Signal{
		Decision:               DecisionEnter,
		Symbol:                 cfg.Symbol,
		Side:                   side,
		EntryPrice:             entry,
		TakeProfitJPY:          cfg.Exit.TakeProfitJPY,
		StopLossJPY:            cfg.Exit.StopLossJPY,
		MaxHoldMinutes:         cfg.Exit.MaxHoldMinutes,
		ExtensionMaxMinutes:    cfg.Exit.ExtensionMaxMinutes,
		ExtensionUnrealizedJPY: cfg.Exit.ExtensionUnrealizedJPY,
		EarlyExitWindowMinutes: cfg.Exit.EarlyExitWindowMinutes,
		EarlyExitTargetJPY:     cfg.Exit.EarlyExitTargetJPY,
		RatchetArmJPY:          cfg.Exit.RatchetArmJPY,
		RatchetGivebackJPY:     cfg.Exit.RatchetGivebackJPY,
		Quantity:               cfg.Risk.Quantity,
		HoldingMode:            cfg.HoldingMode,
		Reason:                 reason,
		ConfigID:               cfg.ConfigID,
		StrategyName:           name,
		CreatedAt:              in.Now,
	}
}

// 出口 ATR 化の事前コミット係数。教科書既定の翻訳(Wilder の 2N ストップ =
// SL、R:R 1.5 = TP)で、過去の損益に当てて選んだ値ではない。**採点結果を見て動かすことは禁止**
// — 動かすなら T を消費する新規検定として事前登録する。円建て固定幅では同じ config が高ボラ銘柄で即死し
// 低ボラ銘柄では一生届かない出口になっていた。
const (
	atrExitPeriod = 14
	atrExitTPMult = 3.0
	atrExitSLMult = 2.0
)

// TP/SL だけを ATR 倍数に差し替える(cfg.Exit の円幅は使わず、MaxHold/EarlyExit/Extension/ratchet は継承)。
// atr <= 0 は呼び手が no_trade("no_atr")に落とす契約 — ここで 0 幅の出口を作らない。
func atrExitSignal(in EvalInput, side order.Side, entry float64, name config.StrategyName, reason string, atr float64) Signal {
	sig := configExitSignal(in, side, entry, name, reason)
	sig.TakeProfitJPY = atrExitTPMult * atr
	sig.StopLossJPY = atrExitSLMult * atr
	return sig
}

// atrExitPeriodATR は出口幾何が使う ATR。**期間を 1 箇所に閉じる** — SL は 2.0×ATR(14)
// なのに ratchet は別の ATR、という静かなズレを作らないため(兄弟アームが使う)。
func atrExitPeriodATR(d []market.Candle) float64 { return ta.ATR(d, atrExitPeriod) }

// 🛑 **Symbol を必ず載せる**。見送りシグナルは長らく銘柄を空のまま返していて、
// 「どの銘柄で見送ったか」が呼出側から復元できなかった。
// `tp_below_cost_floor` を signal_rejections に書けるようにしたときに、
// 銘柄が空の行しか作れないことが判明して是正した(アーム別に数えられない行は
// 書いても検出器として機能しない)。
func noTradeSignal(in EvalInput, name config.StrategyName, reason string) Signal {
	return Signal{
		Decision:     DecisionNoTrade,
		Symbol:       symbolOf(in),
		Reason:       reason,
		StrategyName: name,
		ConfigID:     configID(in),
		CreatedAt:    in.Now,
	}
}

func symbolOf(in EvalInput) string {
	if in.Config == nil {
		return ""
	}
	return in.Config.Symbol
}

func configID(in EvalInput) string {
	if in.Config == nil {
		return ""
	}
	return in.Config.ConfigID
}

// evalGuard は全戦略の Evaluate 共通の入口ガード。config / summary が無い、または
// 日足が minBars 本に満たなければ「見送り」を返す(ok=false)。15 の Evaluate が
// 同じ 7 行を展開していたのを 1 か所に寄せた。
//
// 🛑 理由文字列は signal_rejections の GROUP BY キー(migration 0009)であり、
// 「なぜ今日エントリーしなかったのか」の監査証跡そのもの。**変えない**。
// 🛑 minBars は戦略ごとに違う(ルックバック + 判定に要る余分な本数)。呼び出し側の
// 定数をそのまま渡す — ここに既定値を置くと、履歴不足のまま評価する戦略が出る。
func evalGuard(in EvalInput, name config.StrategyName, minBars int) (Signal, bool) {
	if in.Config == nil || in.Summary == nil {
		return noTradeSignal(in, name, "no_config_or_summary"), false
	}
	if len(in.CandlesDaily) < minBars {
		return noTradeSignal(in, name, "insufficient_daily_history"), false
	}
	return Signal{}, true
}
