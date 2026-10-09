// Package port defines the interfaces the usecase layer depends on.
// R1: port imports domain types but NEVER config — mode and similar values
// cross the boundary as plain strings.
package port

import (
	"context"
	"errors"
	"time"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

// ErrOrderNotFilled is a CONFIRMED zero-fill (ストップ高/安・売買停止・薄商い), distinct
// from an unconfirmable result: the entry saga aborts cleanly (cancel the working
// order, no compensating close, no trip) because there is no position to protect.
var ErrOrderNotFilled = errors.New("broker: order observed working but not filled before deadline")

type KlinePeriod string

const (
	Period1m    KlinePeriod = "1m"
	Period5m    KlinePeriod = "5m"
	Period1h    KlinePeriod = "1h"
	PeriodDaily KlinePeriod = "1d"
)

type PlaceOrderResult struct {
	OrderID          string
	BrokerPositionID string
	Accepted         bool
	Message          string
}

// CloseRequest settles a position (立花: 信用返済 = CashMargin:3).
type CloseRequest struct {
	Symbol           string
	BrokerPositionID string
	Side             order.Side // side of the CLOSE order (opposite of the open)
	Quantity         int
	ExecKind         order.ExecKind
}

type CloseResult struct {
	OrderID  string
	Accepted bool
	// FilledPrice / FeeJPY are 0 when the adapter cannot know them synchronously.
	// Synchronous adapters (paper): the caller falls back to the observed price.
	// Async adapters (SettleFillsAsync() == true, 立花): the caller must confirm the
	// fill via ResolveExecution and must NOT book from the observed price
	// (幽霊決済の禁止 — CLAUDE.md). FeeJPY is added to the frozen entry
	// fee so the daily-loss cap judges NET.
	FilledPrice float64
	FeeJPY      float64
	Message     string
}

type CancelResult struct {
	OrderID   string
	Cancelled bool
}

// BrokerPosition is a broker-side open position (建玉) as polled.
type BrokerPosition struct {
	BrokerPositionID string
	Symbol           string
	Side             order.Side
	Quantity         int
	EntryPrice       float64
	ExecKind         order.ExecKind
}

// Broker is the brokerage-agnostic interface every adapter (paper/tachibana)
// satisfies. RefreshToken re-authenticates across the broker's daily session
// disconnect (立花 API 閉局 03:30); paper implements it as a no-op.
type Broker interface {
	GetTicker(ctx context.Context, symbol string) (*market.Ticker, error)
	GetKlines(ctx context.Context, symbol string, p KlinePeriod, n int) ([]market.Candle, error)
	GetAccountMargin(ctx context.Context) (*order.AccountMargin, error)
	GetPositions(ctx context.Context) ([]BrokerPosition, error)
	GetActiveOrders(ctx context.Context, symbol string) ([]order.Order, error)
	GetExecutions(ctx context.Context, limit int) ([]order.Execution, error)
	PlaceOrder(ctx context.Context, req order.PlaceOrderRequest) (*PlaceOrderResult, error)
	ClosePosition(ctx context.Context, req CloseRequest) (*CloseResult, error)
	CancelOrder(ctx context.Context, orderID string) (*CancelResult, error)
	RefreshToken(ctx context.Context) error
}

// SplitAdjustableBook は**建玉帳をプロセス内に持つ** broker(paper / paper_live_feed)が、
// 株式分割の権利落ちで言い直した建玉(株数・建値)を帳簿へ映す口。実ブローカー(立花)は
// 自分が建玉の正本なので実装しない — live は broker の建玉照会で分割を**確かめる**側。
// ok=false = その id の建玉が帳簿に無い。
type SplitAdjustableBook interface {
	AdjustPositionForSplit(brokerPositionID string, quantity int, entryPrice float64) bool
}

// MarketFeed is the read-only slice of a broker (時価 / 日足 + セッション維持)。
// 発注系メソッドを **含まない** ので受け取る側は型として実弾を送れない —
// paper_live_feed(実フィード × 紙執行)の安全性はこの狭さで担保する。広げない。
type MarketFeed interface {
	GetTicker(ctx context.Context, symbol string) (*market.Ticker, error)
	GetKlines(ctx context.Context, symbol string, p KlinePeriod, n int) ([]market.Candle, error)
	RefreshToken(ctx context.Context) error
}

// ResolvedExecution is a fully-resolved fill. FilledQuantity is authoritative on
// partial fills: OCO/Position freeze on it and the remainder is cancelled.
type ResolvedExecution struct {
	OrderID          string
	BrokerPositionID string
	FilledPrice      float64
	FilledQuantity   int
	FeeJPY           float64
	FilledAt         time.Time
}

type ExecutionResolver interface {
	ResolveExecution(ctx context.Context, orderID string) (ResolvedExecution, error)
}

// SettleFillReporter は「決済(ClosePosition)の約定値が同期に返るか」を宣言する。
// true = 受理しか返らない(立花)ので、呼び手は ResolveExecution で約定を確かめて
// からでないと台帳に書いてはいけない(幽霊決済の禁止)。false = 即約定(paper)。
//
// 🛑 **LiveBroker の一部にしてあるのは意図的**。ラッパ(LiveQuoteShared 等)が
// 転送を忘れると型アサーションが黙って外れ、幽霊決済が復活する。
// interface の一員にしておけば、転送漏れはコンパイルエラーになる。
type SettleFillReporter interface {
	SettleFillsAsync() bool
}

type OCOCloseOrderInput struct {
	Symbol           string
	BrokerPositionID string
	Side             order.Side // side of the CLOSE legs
	Quantity         int
	TakeProfit       float64
	StopLoss         float64
	ExecKind         order.ExecKind

	// ExpireOn は守りの注文をいつまで板に残すか(venue TZ の日付)。zero = 当日限り。
	// 🛑 多日保有では zero にしない。当日期限の守りは**建てた日の引けで消える**ので、
	// 2日目以降の建玉が裸になる。日付の算出(営業日を数える)は呼び出し側の責任 —
	// 休場カレンダーは domain/session が持っており、port は config を知らない(R1)。
	ExpireOn time.Time
}

// OCOCloseOrderPlacer puts TP+SL on the BROKER so the守りが bot 死で消えない。
// OnTick 監視だけの構成は禁止。
type OCOCloseOrderPlacer interface {
	PlaceSettleOCO(ctx context.Context, in OCOCloseOrderInput) (rootOrderID string, err error)
}

type SettleLegResolver interface {
	ResolveSettleLegs(ctx context.Context, brokerPositionID, symbol string) (tpOrderID, slOrderID string, err error)
}

// ProtectiveOrderInfo は板に残っている守りの注文 1 本。ExpireOn が zero = 当日限り
// (または broker が期日を返さない)。**zero を「無期限」と読まない** — 当日限りの
// 守りは引けで消えるので、多日保有では最も危険な状態そのもの。
type ProtectiveOrderInfo struct {
	OrderID  string
	Symbol   string
	ExpireOn time.Time
	// HasStopLeg = 逆指値(SL)脚を持つ。利確指値だけの注文は守りではない。
	HasStopLeg bool
	// Side / Quantity は「この注文は bot の建玉の守りか」を絞るため。
	// 同じ銘柄に人間が置いた注文の期日まで書き換えないこと。
	Side     order.Side
	Quantity int
	// BrokerRef は broker が注文を引くのに要する**追加の鍵**(立花なら営業日)。
	// 🚨 プロセス内 map に頼ると**再起動後に空になる**ので、照会の応答から運ぶ。
	// 空のまま訂正を撃つと別の注文を掴みうる。
	BrokerRef string
	// 🚨 **再発注に要る値段**。訂正では発注日+10営業日の天井を超えられない
	// ので、期日を延ばす唯一の手段が「取消 → 同条件で再発注」になる。
	// そのとき**いま板にある値段をそのまま引き継ぐ**必要がある —— Position の
	// 凍結値を使うと、人間が手で締めた TP/SL を勝手に元の広い幅へ戻してしまう。
	// LimitPrice = 通常脚(利確指値。0 = 無し)/ StopTrigger = 逆指値条件(トリガー価格)
	// / StopPrice = 逆指値の執行値段(0 = 成行)。
	LimitPrice  float64
	StopTrigger float64
	StopPrice   float64
}

// ProtectiveOrderBoard reads and cancels the broker-side protective orders of a
// position. It is the basis of ReplaceProtectiveOrder (cancel → re-place before
// the open) and of closeOne's leg cancel.
//
// 🛑 **LiveBroker の一員にしてあるのは意図的**(SettleFillReporter と同じ理由)。
// ラッパが転送を忘れると型アサーションが黙って外れ、守りが切れるのを誰も
// 検知できなくなる。interface の一員なら転送漏れはコンパイルエラーになる。
//
// 期日を**訂正**する経路(CLMKabuCorrectOrder)は持たない:
// 立花の「10 営業日迄」は発注日起点で、訂正では天井を超えられない。
type ProtectiveOrderBoard interface {
	// ListProtectiveOrders は symbol の板に残っている決済注文(期日つき)。
	ListProtectiveOrders(ctx context.Context, symbol string) ([]ProtectiveOrderInfo, error)
	// CancelProtectiveOrder は守りの注文を取り消す。
	//
	// 🚨 **`CancelOrder(id)` を使わない。** あちらは営業日をプロセス内 map から引き、
	// 空なら "0" に落ちる。守りは前のプロセスが出したものなので、**再起動後の取消は
	// 必ず外れる**。ここも注文そのものを受け取り BrokerRef を使う。
	//
	// 🛑 これが要るのは「訂正では発注日+10営業日の天井を超えられない」から。
	// 期日を延ばす唯一の手段が **取消 → 同条件で再発注** になった。
	CancelProtectiveOrder(ctx context.Context, o ProtectiveOrderInfo) error
}

type LiveBroker interface {
	Broker
	ExecutionResolver
	SettleFillReporter
	OCOCloseOrderPlacer
	SettleLegResolver
	ProtectiveOrderBoard
}
