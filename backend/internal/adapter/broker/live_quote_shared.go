package broker

import (
	"context"
	"sync"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// LiveQuoteShared は hybrid の live トラック用デコレータ。**立花のセッションは口座に
// 1 本**なので、live track も research track と同じ `*Tachibana` と同じ一括クォート
// フィードを共有する(2 プロセス並走は login が互いのセッションを破棄して蹴り合う)。
//
// 3 つのことをする:
//  1. 時価は共有 feed から返す。**これで live track が何銘柄増えても時価レーンの
//     本数は変わらない**(回数はレーン数 × 頻度で決まる)。素通しにすると 1銘柄
//     1リクエストに戻り、`QuotesBatched=false` で価格ループが 30 秒に退避する。
//  2. 建玉 / 余力照会は短 TTL の**併合キャッシュ**に載せる。この 2 つは口座全体の
//     照会を呼び出し側が symbol で絞っているだけなので、素通しだと N 銘柄が同じ
//     質問を N 回投げる(時価の BatchQuoteFeed とまったく同じ構図)。
//  3. **発注系は素通し**。状態変更を畳んだら発注そのものが消える。
//
// 🛑 発注 / 決済 / 取消の直後は口座キャッシュを**捨てる**。畳んだ結果として
// 「建てたのに建玉ゼロ」を返すと、ナンピン禁止ゲートと Reconcile が古い像で判断する。
// 通信量のための併合が守りの誤作動になってはいけない。
type LiveQuoteShared struct {
	tb   port.LiveBroker
	feed port.MarketFeed
	ttl  time.Duration
	c    clock.Clock

	mu        sync.Mutex
	positions accountEntry[[]port.BrokerPosition]
	margin    accountEntry[*order.AccountMargin]
}

// accountEntry は口座照会 1 種ぶんの併合キャッシュ。エラーは**キャッシュしない**
// (古い値を新鮮なふりで返さないのと同じ極性で、失敗は毎回上流に訊き直す)。
type accountEntry[T any] struct {
	val      T
	fetchedA time.Time
	ok       bool
}

func (e accountEntry[T]) fresh(now time.Time, ttl time.Duration) bool {
	return e.ok && now.Sub(e.fetchedA) <= ttl
}

// NewLiveQuoteShared wraps tb so quotes come from feed and account queries merge
// within ttl. ttl<=0 disables the merge (every call reaches tb).
func NewLiveQuoteShared(tb port.LiveBroker, feed port.MarketFeed, ttl time.Duration, c clock.Clock) port.LiveBroker {
	if c == nil {
		c = clock.System()
	}
	return &LiveQuoteShared{tb: tb, feed: feed, ttl: ttl, c: c}
}

// QuotesBatched は feed の申告をそのまま引き継ぐ。ここを固定 true にしてはいけない
// — 一括化されていない feed を被せたときに価格ループが 30 秒へ退避しなくなる。
func (s *LiveQuoteShared) QuotesBatched() bool { return QuotesBatched(s.feed) }

// QuoteChunks も feed の申告を引き継ぐ(監視銘柄が 120 を超えたときの警告用)。
func (s *LiveQuoteShared) QuoteChunks() int { return QuoteChunks(s.feed) }

func (s *LiveQuoteShared) GetTicker(ctx context.Context, symbol string) (*market.Ticker, error) {
	return s.feed.GetTicker(ctx, symbol)
}

func (s *LiveQuoteShared) GetKlines(ctx context.Context, symbol string, p port.KlinePeriod, n int) ([]market.Candle, error) {
	// feed 経由でも同じ tb に届く(BatchQuoteFeed のパススルー)が、feed が
	// 一括化されない実装なら inner がそのまま返る。どちらでも二重セッションは
	// 生まれない。
	return s.feed.GetKlines(ctx, symbol, p, n)
}

func (s *LiveQuoteShared) RefreshToken(ctx context.Context) error {
	return s.feed.RefreshToken(ctx)
}

func (s *LiveQuoteShared) GetPositions(ctx context.Context) ([]port.BrokerPosition, error) {
	now := s.c()
	s.mu.Lock()
	if s.ttl > 0 && s.positions.fresh(now, s.ttl) {
		v := s.positions.val
		s.mu.Unlock()
		return v, nil
	}
	s.mu.Unlock()

	v, err := s.tb.GetPositions(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.positions = accountEntry[[]port.BrokerPosition]{val: v, fetchedA: now, ok: true}
	s.mu.Unlock()
	return v, nil
}

func (s *LiveQuoteShared) GetAccountMargin(ctx context.Context) (*order.AccountMargin, error) {
	now := s.c()
	s.mu.Lock()
	if s.ttl > 0 && s.margin.fresh(now, s.ttl) {
		v := s.margin.val
		s.mu.Unlock()
		return v, nil
	}
	s.mu.Unlock()

	v, err := s.tb.GetAccountMargin(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.margin = accountEntry[*order.AccountMargin]{val: v, fetchedA: now, ok: true}
	s.mu.Unlock()
	return v, nil
}

// invalidateAccount は口座の像を捨てる。状態を変えたのに古い像を返すのは、
// 通信量の節約ではなく守りの故障。
func (s *LiveQuoteShared) invalidateAccount() {
	s.mu.Lock()
	s.positions = accountEntry[[]port.BrokerPosition]{}
	s.margin = accountEntry[*order.AccountMargin]{}
	s.mu.Unlock()
}

// --- 以下はすべて素通し。発注系は決してキャッシュしない。 ---

func (s *LiveQuoteShared) GetActiveOrders(ctx context.Context, symbol string) ([]order.Order, error) {
	return s.tb.GetActiveOrders(ctx, symbol)
}

func (s *LiveQuoteShared) GetExecutions(ctx context.Context, limit int) ([]order.Execution, error) {
	return s.tb.GetExecutions(ctx, limit)
}

func (s *LiveQuoteShared) PlaceOrder(ctx context.Context, req order.PlaceOrderRequest) (*port.PlaceOrderResult, error) {
	defer s.invalidateAccount()
	return s.tb.PlaceOrder(ctx, req)
}

func (s *LiveQuoteShared) ClosePosition(ctx context.Context, req port.CloseRequest) (*port.CloseResult, error) {
	defer s.invalidateAccount()
	return s.tb.ClosePosition(ctx, req)
}

func (s *LiveQuoteShared) CancelOrder(ctx context.Context, orderID string) (*port.CancelResult, error) {
	defer s.invalidateAccount()
	return s.tb.CancelOrder(ctx, orderID)
}

// 🛑 転送を忘れると呼び手の型アサーションが黙って外れ、決済の約定を確認しないまま
// 台帳に書く経路(幽霊決済)が復活する。
// port.LiveBroker の一員にして**忘れたらコンパイルが通らない**ようにしてある。
func (s *LiveQuoteShared) SettleFillsAsync() bool { return s.tb.SettleFillsAsync() }

func (s *LiveQuoteShared) ResolveExecution(ctx context.Context, orderID string) (port.ResolvedExecution, error) {
	return s.tb.ResolveExecution(ctx, orderID)
}

func (s *LiveQuoteShared) PlaceSettleOCO(ctx context.Context, in port.OCOCloseOrderInput) (string, error) {
	defer s.invalidateAccount()
	return s.tb.PlaceSettleOCO(ctx, in)
}

func (s *LiveQuoteShared) ResolveSettleLegs(ctx context.Context, brokerPositionID, symbol string) (string, string, error) {
	return s.tb.ResolveSettleLegs(ctx, brokerPositionID, symbol)
}

var _ port.LiveBroker = (*LiveQuoteShared)(nil)

// 🛑 転送を忘れると守りの置き直しが誰にも走らないまま切れる。interface の一員に
// してあるので、この 2 本を消すとコンパイルが通らない(port.ProtectiveOrderBoard)。
func (s *LiveQuoteShared) ListProtectiveOrders(ctx context.Context, symbol string) ([]port.ProtectiveOrderInfo, error) {
	return s.tb.ListProtectiveOrders(ctx, symbol)
}

func (s *LiveQuoteShared) CancelProtectiveOrder(ctx context.Context, o port.ProtectiveOrderInfo) error {
	return s.tb.CancelProtectiveOrder(ctx, o)
}
