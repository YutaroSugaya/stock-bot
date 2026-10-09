package command

import (
	"fmt"
	"strings"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

// ArmTemplate は **(銘柄, 枠の戦略, 直近終値)** から arm 可能な `StrategyConfig` を
// 組み立てる決定論の関数。LLM を発注経路から外した後の唯一の config 生成元。
//
// 🚨 なぜ LLM を外したか(実測): LLM が書く全フィールドを
// 機械的に集計した結果、**実行に効く変動する出力は 1 つも無かった**。生きた出力は
// `strategy_name`(構える / 構えない の 1 ビット)だけで、それも実測で **97% が「構える」**。
// `take_profit_jpy` / `stop_loss_jpy` は 9 戦略すべてが自分で出口を計算して上書きするので
// **未使用**、`market_regime` は Go からの参照ゼロだった。
//
// 🛑 これは LLM の性能への評価ではなく **audition 設計の論理的な帰結**。9 戦略を公平に
// 測るとは戦略以外の要因を取り除くことで、LLM の裁量は戦略以外の要因(= 交絡)だから、
// 公平性を追求するほど裁量は必然的に剥がれる。
//
// 🛑 **advisor のコードとプロンプトは消さない**。これは「measurement の間の
// 一時撤去」で、戦略が live allowlist に入った後の日次 go/no-go には正当な用途が残る。
type ArmTemplate struct {
	HardLimits *config.HardLimits
	Mode       config.Mode
	// ExecKindFor は holding_mode から**実際に執行される区分**を返す(bot_config の
	// holding ブロック)。凍結値をここから取るので、台帳の exec_kind と実際の発注が
	// 食い違わない。nil = 現物。
	ExecKindFor func(order.HoldingMode) order.ExecKind
}

// 事前登録の定数。**変えるときは事前登録を更新する**。
const (
	armQuantity              = 100
	armMaxOpenPositions      = 1
	armMaxTradesInThisWindow = 3
	armMaxSpreadTicks        = 5.0
	armTTLMinutes            = 0

	// 窓損失 cap = 直近終値 × これ × quantity × 3(= 3 敗ぶん)。0.07 は
	// 2.0×ATR の終値比の実測中央値で、**`stop_loss_jpy` から計算しない** —
	// 実際の 1 敗は ATR 由来なので、旧式だと 3 敗ぶんのつもりが 1 敗で発動する。
	armLossPerTradePct = 0.07
	armWindowLosses    = 3
)

// Build は 1 つの (銘柄, 戦略) 枠に対する config を返す。同じ入力なら**同じ結果**。
func (a *ArmTemplate) Build(symbol string, name config.StrategyName, lastClose float64, now time.Time) (*config.StrategyConfig, error) {
	if lastClose <= 0 {
		// 価格が無いと窓損失 cap を検証できない。値を捏造せず fail-close。
		return nil, fmt.Errorf("arm template: %s の直近終値が無い(fail-close)", symbol)
	}
	holding := order.HoldingMultiday
	if strategy.EntryArmOf(name) == config.StrategyBNFIntradayReversion {
		// 分岐はこの 1 つだけ。`_trail` 兄弟は入口へ
		// 畳んで同じ側に落とす — 片方だけ multiday になると引け前フラット化から漏れる。
		holding = order.HoldingIntraday
	}
	execKind := order.ExecCash
	if a.ExecKindFor != nil {
		execKind = a.ExecKindFor(holding)
	}

	c := &config.StrategyConfig{
		Symbol:       symbol,
		StrategyName: name,
		Mode:         a.Mode,
		HoldingMode:  holding,
		ExecKind:     execKind,
		TTLMinutes:   armTTLMinutes,
	}
	c.Entry.Direction = ArmDirection(name)
	c.Entry.MaxSpreadTicks = armMaxSpreadTicks
	// 出口は 9 戦略すべてが ATR から自分で算出して上書きする。ここに数字を
	// 置くと「使われない値が台帳に凍る」ので 0 のままにする。MaxHold も戦略側
	// (strategy.MaxHoldBusinessDays)が Signal に載せる。
	c.Risk.Quantity = armQuantity
	c.Risk.MaxOpenPositions = armMaxOpenPositions
	c.Risk.MaxTradesInThisWindow = armMaxTradesInThisWindow
	c.Risk.MaxLossInThisWindowJPY = a.windowLossJPY(lastClose)

	// config_id は **内容に縛る**(config 凍結: 同じ id は永久に同じ内容)。日付を頭に
	// 付けるのは人間が台帳を読むときの手掛かり。Fingerprint は symbol / config_id /
	// provenance を含まないので、同じテンプレを 200 銘柄へ複製しても同じ指紋になる。
	c.ConfigID = fmt.Sprintf("det-%s-%s-%s-%s",
		now.Format("20060102"), symbol, strings.ReplaceAll(string(name), "_", ""), c.Fingerprint())
	return c, nil
}

// 窓損失 cap。式の値が hard limit の下限を割る(極端に安い銘柄)ときは**下限へ寄せる** —
// そのままだと config ごと reject され、標本が静かに落ちる。寄せる向きは cap を**緩める**
// 側なので、censoring を増やすことはない。
func (a *ArmTemplate) windowLossJPY(lastClose float64) int {
	v := int(lastClose * armLossPerTradePct * armQuantity * armWindowLosses)
	if a.HardLimits != nil && a.HardLimits.MaxLossInWindowJPY.Min > 0 && v < a.HardLimits.MaxLossInWindowJPY.Min {
		return a.HardLimits.MaxLossInWindowJPY.Min
	}
	return v
}

// ArmDirection は戦略ごとの**事前登録された向き**。値はカタログ
// (`strategy.Catalog` の Direction 列)が持ち、`_trail` 兄弟は基のアームの値を引く。
//
// `both` の 4 入口(abs_momentum_v2 / atr_breakout_v2 / donchian_breakout_v2 / high_52w_momentum)は
// **入口の式が既に両向きで書かれている**ので、`direction` を開けるだけで空売り標本が取れる。
// bnf 家族は long-only(鏡像は台帳 #18 で reject 済み)、high_volume_premium / post_jump_drift は
// 買いを直接渡している。
func ArmDirection(name config.StrategyName) config.Direction {
	return strategy.EntryDirection(name)
}

// 兄弟アームを入口へ畳む規則は domain 側(strategy.EntryArmOf)が正本。ここで別の
// リテラルを持つと、片方だけ直したときに **direction と MaxHold が食い違う**。
