package port

import (
	"context"
	"time"
)

// 日次総評の段1が読む面。**read-only** で、書込メソッドを
// 型として持たない — このバイナリから台帳を触れない構造にするため。
//
// 🛑 これは**日記の材料**であってエッジの証拠ではない。撤退・継続・ロットの判定は
// 事前コミット基準と `cmd/edge-judge` が出す。

// JournalTrade は当日 closed になった 1 往復。net は下流で gross − fee + carry。
type JournalTrade struct {
	Symbol      string
	Strategy    string
	Side        string
	Quantity    int
	EntryPrice  float64
	ClosePrice  float64
	GrossJPY    float64
	FeeJPY      float64
	CarryJPY    float64
	CloseReason string
	ClosedAt    time.Time
}

func (t JournalTrade) NetJPY() float64 { return t.GrossJPY - t.FeeJPY + t.CarryJPY }

// JournalPosition は建玉 1 本(当日 OPEN になったもの / 引け時点で開いているもの)。
type JournalPosition struct {
	Symbol     string
	Strategy   string
	ConfigID   string
	Side       string
	Quantity   int
	EntryPrice float64
	OpenedAt   time.Time

	// 🚨 **凍結した出口幾何と、走っている極値**。
	// これが無いと日記は「台帳を見れば分かること」しか書けず、**その建玉が
	// N 日目にどこにいたか**が永久に失われる。台帳の MFE/MAE は*走っている極値*
	// であって時系列ではないので、日次スナップショットだけが経路を作る。
	// 単位: Price は絶対価格、Peak/Trough は **円/株**(CLAUDE.md の凍結規約と同じ)。
	TakeProfitPrice     float64
	StopLossPrice       float64
	PeakUnrealizedJPY   float64 // MFE(建値からの最大含み・円/株)
	TroughUnrealizedJPY float64 // MAE(建値からの最大逆行・円/株)
}

// JournalRejection は「なぜ入らなかったか」の当日集計(理由の安定種別ごと)。
type JournalRejection struct {
	Reason string
	N      int
}

// JournalScreen は当日の戦略別トリガー数と、枠を得た数。
type JournalScreen struct {
	Strategy   string
	Triggered  int
	Picked     int
	TopSymbols []string // score 上位(最大 5)
}

// JournalSource は段1 の台帳側の口。期間は [from, to) の半開区間(JST 日の境界)。
type JournalSource interface {
	JournalClosedTrades(ctx context.Context, from, to time.Time) ([]JournalTrade, error)
	JournalOpenedPositions(ctx context.Context, from, to time.Time) ([]JournalPosition, error)
	JournalOpenPositionsAsOf(ctx context.Context, at time.Time) ([]JournalPosition, error)
	JournalRejections(ctx context.Context, from, to time.Time) ([]JournalRejection, error)
	JournalScreens(ctx context.Context, from, to time.Time) ([]JournalScreen, error)
}

// ClosedPositionSnapshot は決済済み建玉の**凍結された出口幾何**。反実仮想
// (「そのまま持っていたら TP と SL のどちらに当たっていたか」)の入力で、
// 建玉時に凍結した絶対価格をそのまま使う(config の現在値ではない)。
type ClosedPositionSnapshot struct {
	PositionID      int64
	Symbol          string
	Strategy        string
	Side            string
	Quantity        int
	EntryPrice      float64
	TakeProfitPrice float64
	StopLossPrice   float64
	ClosePrice      float64
	NetJPY          float64
	// RatchetArmJPY > 0 = トレール建玉。実際の出口は peak − giveback の線であって
	// 固定 TP/SL ではないので、反実仮想の結果を他戦略と同列に読めない。
	RatchetArmJPY float64
	// RatchetGivebackJPY / PeakUnrealizedJPY / TroughUnrealizedJPY は
	// **「決済時の peak 分布」と「床あり再計算」の材料**。
	//
	// 🚨 これが無いと ①トレールが armed に届いたのか届かなかったのかが読めず
	// ②床(`max(arm, peak − giveback)`)を後から当て直せない。事前登録は採点時に必読と
	// 宣言しているのに、読む道具がどこにも無かった。
	RatchetGivebackJPY  float64
	PeakUnrealizedJPY   float64 // MFE(円/株)
	TroughUnrealizedJPY float64 // MAE(円/株)
	// RatchetFloorAtArm は**その建玉にトレールの床が効いていたか**の版スタンプ。
	// false の建玉(床の導入前の旧建玉)に床を当て直して読むと、測っている
	// 対象を後から入れ替えることになる。
	RatchetFloorAtArm bool
	CloseReason       string
	// MaxHoldMinutes は**凍結された**保有上限(0 = 無期限)。
	//
	// 🚨 反実仮想がこれを見ないと、`manual` / `forced_flat` で打ち切った建玉を
	// 「MaxHold も無かったら」で歩いてしまい、期限の後に来た TP/SL を拾って
	// **存在しない出口**を数える。config 系の戦略にも期限が付いているので、
	// これが無いと実質全件がずれる。
	MaxHoldMinutes int
	OpenedAt       time.Time
	ClosedAt       time.Time
}

// CounterfactualSource は反実仮想が読む面(read-only)。
type CounterfactualSource interface {
	// ClosedPositions は closed_at が [from, to) で、close_reason が reasons に
	// 含まれる決済済み建玉を返す。reasons が空なら全件。
	ClosedPositions(ctx context.Context, from, to time.Time, reasons []string) ([]ClosedPositionSnapshot, error)
}
