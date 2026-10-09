// Package position は bot が持つ建玉。出口幾何 + HoldingMode + ExecKind は entry 時に凍結され
// 以後不変(config-freeze 不変条件)。
package position

import (
	"time"

	"stockbot/backend/internal/domain/order"
)

// CLOSING は決済判断から約定記帳までの CAS 済み in-flight 状態。
type Status string

const (
	StatusOpen    Status = "OPEN"
	StatusClosing Status = "CLOSING"
	StatusClosed  Status = "CLOSED"
	StatusUnknown Status = "UNKNOWN"
)

// external_broker は reconcile で採用した建玉。表示と委託保証金には効くが bot は管理しない。
type Source string

const (
	SourceBot      Source = "bot"
	SourceManual   Source = "manual"
	SourceExternal Source = "external_broker"
)

type Position struct {
	ID               int64
	BrokerPositionID string
	Symbol           string
	Side             order.Side
	Quantity         int
	EntryPrice       float64

	// 凍結出口(単位は円/株 と分)。tick 建てにしない理由: 呼値は銘柄と価格帯で 0.1〜10円に変わるので、
	// 同じ「50 tick」が銘柄ごとに別の賭けになる。呼値は発注価格をグリッドに丸めるためだけに使う。
	TakeProfitJPY          float64 // 建値からの利確幅(円/株)。0 = 無し
	StopLossJPY            float64 // 建値からの損切幅(円/株)。0 = 無し
	MaxHoldMinutes         int
	ExtensionMaxMinutes    int
	ExtensionUnrealizedJPY float64 // 延長を許す含み(円/株)の絶対値
	EarlyExitWindowMinutes int
	EarlyExitTargetJPY     float64
	RatchetArmJPY          float64
	RatchetGivebackJPY     float64

	// 建玉時に確定した絶対価格(呼値グリッド丸め済み)。broker OCO へそのまま送り EvaluateExit も直接比べる。
	// 0 = 未設定(外部採用建玉など)→ その脚は評価しない。
	TakeProfitPrice float64
	StopLossPrice   float64

	// Runtime excursion state, updated by ManageOpenPositions.OnTick for EVERY
	// position (not just ratchet ones): peak = MFE, trough = MAE, both in 円/株.
	// They drive no exit rule outside ratchet — they are the record that lets a
	// trail (or a wider SL) be evaluated counterfactually after the fact.
	PeakUnrealizedJPY   float64
	TroughUnrealizedJPY float64
	RatchetArmed        bool

	// RatchetFloorAtArm は「トレールの床が効く建玉か」を **entry 時に凍結**する。
	// 床は Position の凍結値から**実行時に**計算されるので、フラグが無いとコード変更が
	// 床の導入前に建てた旧建玉にも**その場で**効いてしまい、
	// 測っている対象そのものが途中で入れ替わる。**新規建玉だけ true**。
	RatchetFloorAtArm bool

	StrategyConfigID string
	// StrategyName は凍結 config の戦略名の写し(migration 0015)。ナンピン禁止のキーが
	// (銘柄, 側, **戦略**)になったので、エントリー経路が 3秒ごとに引く必要がある —
	// strategy_configs への join を毎ティック足すのは払えない。
	// 🛑 **"" = 戦略不明**(strategy_name を持たない旧建玉と external 建玉)。ナンピン判定では
	// 「どの戦略に対しても数える」側に倒す — 「戦略が違う」と読むと二重に持つ。
	// 型は素の string(R1: port → config を import しないので config.StrategyName は置けない)。
	StrategyName    string
	HoldingMode     order.HoldingMode
	ExecKind        order.ExecKind
	TickSizeAtEntry float64
	Source          Source
	Status          Status
	OpenedAt        time.Time
	ClosedAt        *time.Time

	// OPEN 脚の手数料(entry 時に凍結)。close trade 行へ引き継ぎ、daily-loss cap を net で判定するために要る。
	EntryFeeJPY float64

	// SplitFactor は建玉時からの**累積の株数倍率**(1:5 分割なら 5。migration 0022)。
	// 0 / 1 = 分割調整なし。SplitAdjustedOn は最後に調整した権利落ち日(zero = なし)で、
	// **同じ日に二度割らない**ための印(再起動をまたいでも権利落ちの判定は毎ティック成立する)。
	// 詳細は SplitAdjusted。
	SplitFactor     float64
	SplitAdjustedOn time.Time
}

func (p Position) MaxHoldUntil() time.Time {
	if p.MaxHoldMinutes <= 0 {
		return time.Time{}
	}
	return p.OpenedAt.Add(time.Duration(p.MaxHoldMinutes) * time.Minute)
}

// 含み損益(円/株・side 符号付き)。呼値は関与しない。
func (p Position) UnrealizedJPY(price float64) float64 {
	diff := price - p.EntryPrice
	if p.Side == order.SideSell {
		diff = -diff
	}
	return diff
}

// UnrealizedJPY の逆写像(ratchet の giveback 線などを価格へ戻すのに使う)。
func (p Position) PriceForUnrealized(unreal float64) float64 {
	if p.Side == order.SideSell {
		return p.EntryPrice - unreal
	}
	return p.EntryPrice + unreal
}
