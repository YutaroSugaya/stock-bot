// Package order は売買・約定まわりの値型(broker 非依存)。
package order

import "time"

type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

func (s Side) Valid() bool { return s == SideBuy || s == SideSell }

// 不正な side では既定 BUY に倒さず空文字を返し、下流の Valid() で reject させる(誤った反対売買を防ぐ)。
func (s Side) Opposite() Side {
	switch s {
	case SideBuy:
		return SideSell
	case SideSell:
		return SideBuy
	default:
		return Side("")
	}
}

// IFDOCO は entry+TP+SL の単発形(broker 依存)。MARKET は二段構えの fallback。
type OrderType string

const (
	OrderTypeMarket OrderType = "MARKET"
	OrderTypeLimit  OrderType = "LIMIT"
	OrderTypeStop   OrderType = "STOP"
	OrderTypeOCO    OrderType = "OCO"
	OrderTypeIFDOCO OrderType = "IFDOCO"
)

type PlaceOrderRequest struct {
	Symbol     string
	Side       Side
	Type       OrderType
	Quantity   int
	Price      float64  // 0 for market
	TakeProfit float64  // 0 if absent (broker-side TP price, tick-rounded)
	StopLoss   float64  // 0 if absent (broker-side SL price, tick-rounded)
	ExecKind   ExecKind // 現物/信用 区分; adapters map to their native field (empty = adapter default)
	ClientTag  string   // optional dedupe / tracing tag
}

type Order struct {
	OrderID   string
	Symbol    string
	Side      Side
	Type      OrderType
	Quantity  int
	Price     float64
	Status    string
	CreatedAt time.Time
	// HasStopLeg は「この注文が逆指値(SL)脚を持つか」。決済側に注文があること自体は
	// 守りの証明にならない — 利確指値だけが残っている建玉は下方向に裸。broker 中立な
	// 真偽値にしてあるのは、判別子の綴り(立花なら sOrderGyakusasiOrderType)を
	// domain に持ち込まないため。
	HasStopLeg bool
}

// FeeJPY は「正 = コスト」、CarryJPY(配当落調整金 / 貸株料 / 金利)は「正 = 受取」に正規化。
type Execution struct {
	ExecutionID string
	OrderID     string
	PositionID  string
	Symbol      string
	Side        Side
	Quantity    int
	Price       float64
	Timestamp   time.Time

	FeeJPY      float64
	CarryJPY    float64
	LossGainJPY float64
}

// risk チェック入力。MarginRatio = 維持率、AvailableJPY = 取引可能額。
type AccountMargin struct {
	// AvailableJPY は**現物**の買付余力。信用注文の可否はこれでは判定できない。
	AvailableJPY float64
	MarginRatio  float64
	Equity       float64
	// MarginNewJPY は**信用新規建可能額**。現物余力とは別枠で、信用注文の
	// collateral ゲートはこちらを見る(現物余力で判定していて
	// 「新規建余力は0円です(最低保証金割れ)」を事前に止められなかった)。
	// 追証中は 0 に倒す — 追証で建て増すのは最悪の行動。
	MarginNewJPY float64
	// MarginCall は追証確定フラグ。維持率割れの trip とは**別軸**の停止条件。
	MarginCall bool
}
