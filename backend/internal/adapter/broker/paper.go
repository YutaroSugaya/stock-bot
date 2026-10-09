package broker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// MARKET は現在値 + 不利方向スリッページで即約定するインメモリ約定エンジン(コスト下限のモデル)。
type Paper struct {
	mu             sync.Mutex
	clock          clock.Clock
	slippageTicks  float64
	feeJPYPerTrade float64

	prices     map[string]float64
	positions  map[string]port.BrokerPosition
	tpsl       map[string]ocoLegs // brokerPositionID -> 守りの leg
	orders     map[string]order.Order
	executions []order.Execution
	resolved   map[string]port.ResolvedExecution
	seq        int64

	// 0 = 既定100万円。研究モードでは collateral ゲートが entry を絞ってサンプルを censoring しないよう大きくする。
	BalanceJPY int
}

type ocoLegs struct {
	rootOrderID          string
	tpOrderID, slOrderID string
	tp, sl               float64
}

func NewPaper(c clock.Clock, slippageTicks, feeJPYPerTrade float64) *Paper {
	if c == nil {
		c = clock.System()
	}
	return &Paper{
		clock:          c,
		slippageTicks:  slippageTicks,
		feeJPYPerTrade: feeJPYPerTrade,
		prices:         make(map[string]float64),
		positions:      make(map[string]port.BrokerPosition),
		tpsl:           make(map[string]ocoLegs),
		orders:         make(map[string]order.Order),
		resolved:       make(map[string]port.ResolvedExecution),
	}
}

// 起動時に耐久台帳から紙の建玉帳を復元する(冪等)。紙の帳簿はプロセス内 map なので `make stop/start` で
// 消えるが Postgres の positions は OPEN のまま残る。復元しないと再起動を跨いだ建玉で
//  1. ClosePosition が "position not found" → 守りを外した直後の reject 扱いで emergency trip し、
//     行は CLOSING のまま座礁する(paper は reconcile 非実行)
//  2. 採番 seq が毎プロセス 1 から始まるので新しい建玉が古い broker_position_id を再利用し、
//     TP 時に**別銘柄の建玉**を決済して台帳へ別銘柄の値段が記録される
//
// が起きる。台帳(trades)は forward 検証の唯一のエッジ証拠なので 2 は許容できない。
// 🛑 seq は復元した id の最大採番より必ず先へ進めること(2 の根治)。
func (p *Paper) AdoptOpenPositions(ps []port.BrokerPosition) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	added := 0
	for _, bp := range ps {
		if bp.BrokerPositionID == "" {
			continue // external adoption 等: 紙の帳簿に対応する建玉が無い
		}
		if n, ok := idSeq(bp.BrokerPositionID); ok && n > p.seq {
			p.seq = n
		}
		if _, exists := p.positions[bp.BrokerPositionID]; exists {
			continue
		}
		p.positions[bp.BrokerPositionID] = bp
		added++
	}
	return added
}

// AdjustPositionForSplit は権利落ちで言い直した建玉を紙の帳簿へ映す(port.SplitAdjustableBook)。
// 台帳(positions)が正本で、帳簿は決済時の約定記録の株数に使われるだけだが、揃えておかないと
// 約定照会が分割前の株数を返し続ける。
func (p *Paper) AdjustPositionForSplit(brokerPositionID string, quantity int, entryPrice float64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	bp, ok := p.positions[brokerPositionID]
	if !ok {
		return false
	}
	bp.Quantity, bp.EntryPrice = quantity, entryPrice
	p.positions[brokerPositionID] = bp
	return true
}

func idSeq(id string) (int64, bool) {
	i := strings.LastIndexByte(id, '-')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(id[i+1:], 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func (p *Paper) SetPrice(symbol string, price float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prices[symbol] = price
}

func (p *Paper) nextID(prefix string) string {
	p.seq++
	return fmt.Sprintf("%s-%d", prefix, p.seq)
}

// 設定価格の周りに 1 tick 幅の合成 bid/ask を作る。
func (p *Paper) GetTicker(_ context.Context, symbol string) (*market.Ticker, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	price, ok := p.prices[symbol]
	if !ok || price <= 0 {
		return nil, fmt.Errorf("paper: no price set for %q", symbol)
	}
	ts := market.TickSizeOf(symbol, price)
	return &market.Ticker{
		Symbol:    symbol,
		Bid:       price - ts/2,
		Ask:       price + ts/2,
		Last:      price,
		Timestamp: p.clock(),
	}, nil
}

// paper に履歴は無い(日足が要る戦略には candle repository / backtest 経路から与える)。
func (p *Paper) GetKlines(_ context.Context, _ string, _ port.KlinePeriod, _ int) ([]market.Candle, error) {
	return nil, nil
}

func (p *Paper) GetAccountMargin(_ context.Context) (*order.AccountMargin, error) {
	bal := 1_000_000.0
	if p.BalanceJPY > 0 {
		bal = float64(p.BalanceJPY)
	}
	// 🛑 **MarginNewJPY も返す**。信用注文の collateral ゲートはこちらを見るので、
	// 0 のままだと paper の信用エントリーが全て insufficient_margin で落ちる
	// (research は margin_system で回っているので forward 収集が丸ごと止まる)。
	// 紙に信用の建余力という概念は無いので、現物余力と同じ値にして**挙動を
	// 変えない**のが正しい — paper に新しい制約を足すのが目的ではない。
	return &order.AccountMargin{AvailableJPY: bal, MarginRatio: 1.0, Equity: bal, MarginNewJPY: bal}, nil
}

func (p *Paper) GetPositions(_ context.Context) ([]port.BrokerPosition, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]port.BrokerPosition, 0, len(p.positions))
	for _, pos := range p.positions {
		out = append(out, pos)
	}
	return out, nil
}

// entry は即 FILLED なので実質は守りの OCO leg。live と同じ reconcile 検査を paper でも通すため。
func (p *Paper) GetActiveOrders(_ context.Context, symbol string) ([]order.Order, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []order.Order
	for _, o := range p.orders {
		if o.Symbol == symbol && o.Status != "FILLED" {
			out = append(out, o)
		}
	}
	return out, nil
}

func (p *Paper) GetExecutions(_ context.Context, limit int) ([]order.Execution, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.executions)
	if limit > 0 && limit < n {
		return append([]order.Execution(nil), p.executions[n-limit:]...), nil
	}
	return append([]order.Execution(nil), p.executions...), nil
}

// 不利方向のスリッページ: BUY は高く、SELL は安く約定させる。
// 🛑 価格を知らない銘柄は 0 を返す(スリッページを足して正の値段を捏造しない)。
func (p *Paper) fillPrice(symbol string, side order.Side) float64 {
	price := p.prices[symbol]
	if price <= 0 {
		return 0
	}
	slip := p.slippageTicks * market.TickSizeOf(symbol, price)
	if side == order.SideBuy {
		return market.RoundToTickOf(symbol, price+slip)
	}
	return market.RoundToTickOf(symbol, price-slip)
}

func (p *Paper) PlaceOrder(_ context.Context, req order.PlaceOrderRequest) (*port.PlaceOrderResult, error) {
	if !req.Side.Valid() {
		return nil, fmt.Errorf("paper: invalid side %q", req.Side)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.prices[req.Symbol]; !ok {
		return nil, fmt.Errorf("paper: no price for %q", req.Symbol)
	}
	now := p.clock()
	orderID := p.nextID("ord")
	bpID := p.nextID("bp")
	fill := p.fillPrice(req.Symbol, req.Side)

	p.orders[orderID] = order.Order{OrderID: orderID, Symbol: req.Symbol, Side: req.Side, Type: req.Type, Quantity: req.Quantity, Price: fill, Status: "FILLED", CreatedAt: now}
	p.positions[bpID] = port.BrokerPosition{BrokerPositionID: bpID, Symbol: req.Symbol, Side: req.Side, Quantity: req.Quantity, EntryPrice: fill}
	p.executions = append(p.executions, order.Execution{
		ExecutionID: p.nextID("ex"), OrderID: orderID, PositionID: bpID, Symbol: req.Symbol,
		Side: req.Side, Quantity: req.Quantity, Price: fill, Timestamp: now, FeeJPY: p.feeJPYPerTrade,
	})
	p.resolved[orderID] = port.ResolvedExecution{OrderID: orderID, BrokerPositionID: bpID, FilledPrice: fill, FilledQuantity: req.Quantity, FeeJPY: p.feeJPYPerTrade, FilledAt: now}
	return &port.PlaceOrderResult{OrderID: orderID, BrokerPositionID: bpID, Accepted: true}, nil
}

func (p *Paper) ClosePosition(_ context.Context, req port.CloseRequest) (*port.CloseResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos, ok := p.positions[req.BrokerPositionID]
	if !ok {
		return &port.CloseResult{Accepted: false, Message: "position not found"}, nil
	}
	// 🚨 価格を知らない銘柄の決済は FilledPrice=0 で返す(fillPrice が 0 を返す)。
	// 呼び手は観測価格へ fallback するか、価格が無ければ CLOSING のまま defer する。
	// fillPrice が 0 に**スリッページを足す**形だと、売建の
	// 返済(買い)が +2 円で「確定約定」し、建値 3,000 の空売りに +30 万円の架空勝ちが
	// 研究台帳へ載る経路になる(再起動直後に CLOSING を再発行するとき価格 map が空)。
	now := p.clock()
	orderID := p.nextID("ord")
	fill := p.fillPrice(pos.Symbol, pos.Side.Opposite())
	p.executions = append(p.executions, order.Execution{
		ExecutionID: p.nextID("ex"), OrderID: orderID, PositionID: pos.BrokerPositionID, Symbol: pos.Symbol,
		Side: pos.Side.Opposite(), Quantity: pos.Quantity, Price: fill, Timestamp: now, FeeJPY: p.feeJPYPerTrade,
	})
	delete(p.positions, req.BrokerPositionID)
	if legs, ok := p.tpsl[req.BrokerPositionID]; ok {
		delete(p.orders, legs.rootOrderID) // 守りの OCO は建玉と一緒に消える
		delete(p.tpsl, req.BrokerPositionID)
	}
	return &port.CloseResult{OrderID: orderID, Accepted: true, FilledPrice: fill, FeeJPY: p.feeJPYPerTrade}, nil
}

// paper は即約定なので取消は常に成功の no-op。
func (p *Paper) CancelOrder(_ context.Context, orderID string) (*port.CancelResult, error) {
	return &port.CancelResult{OrderID: orderID, Cancelled: true}, nil
}

func (p *Paper) RefreshToken(_ context.Context) error { return nil }

// paper は即約定なので、決済の約定は照会するものではない = live 用の約定解決の
// 経路に入れない(呼び手は従来どおり観測価格へ fallback する)。
//
// ⚠ 「必ず価格を返す」わけではない: 価格 map に無い銘柄では FilledPrice=0 を返す
// (PlaceOrder と違って price の有無を確認しない)。再起動直後で当日ずっと未約定の
// 銘柄がこれに当たる。呼び手は観測価格へ fallback する(closeOne)か、価格が無ければ
// CLOSING のまま defer する(reconcile)。0 にスリッページを足した
// 正の値段を返してはいけない。
func (p *Paper) SettleFillsAsync() bool { return false }

func (p *Paper) ResolveExecution(_ context.Context, orderID string) (port.ResolvedExecution, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.resolved[orderID]
	if !ok {
		return port.ResolvedExecution{}, fmt.Errorf("paper: no resolved execution for %q", orderID)
	}
	return r, nil
}

// live と同じく WORKING の決済注文として板にも出す(paper でも reconcile の守り検査を通すため)。
func (p *Paper) PlaceSettleOCO(_ context.Context, in port.OCOCloseOrderInput) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.positions[in.BrokerPositionID]; !ok {
		return "", fmt.Errorf("paper: cannot place OCO, no position %q", in.BrokerPositionID)
	}
	root := p.nextID("oco")
	p.tpsl[in.BrokerPositionID] = ocoLegs{rootOrderID: root, tpOrderID: root + "-tp", slOrderID: root + "-sl", tp: in.TakeProfit, sl: in.StopLoss}
	p.orders[root] = order.Order{
		OrderID: root, Symbol: in.Symbol, Side: in.Side, Quantity: in.Quantity,
		Price: in.StopLoss, Status: "WORKING", CreatedAt: p.clock(), HasStopLeg: true,
	}
	return root, nil
}

// paper の守りに期日は無い(プロセス内の帳簿で、引けで消えたりしない)。**期日を
// 持たない**ことを true で偽装せず、板に残る注文を素直に返して「延長は不要」を
// zero 件で表す。
func (p *Paper) ListProtectiveOrders(_ context.Context, _ string) ([]port.ProtectiveOrderInfo, error) {
	return nil, nil
}

// 紙には期日という概念が無いので、取り消す守りも無い。**エラーにしない** ——
// paper トラックで期日更新の経路を回しても no-op で素通りするのが正しい。
func (p *Paper) CancelProtectiveOrder(_ context.Context, _ port.ProtectiveOrderInfo) error {
	return nil
}

func (p *Paper) ResolveSettleLegs(_ context.Context, brokerPositionID, _ string) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	legs, ok := p.tpsl[brokerPositionID]
	if !ok {
		return "", "", fmt.Errorf("paper: no OCO legs for %q", brokerPositionID)
	}
	return legs.tpOrderID, legs.slOrderID, nil
}

var _ port.LiveBroker = (*Paper)(nil)
var _ port.SplitAdjustableBook = (*Paper)(nil)
