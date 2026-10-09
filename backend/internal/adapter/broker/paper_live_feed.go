package broker

import (
	"context"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// 実フィード(read-only)× 紙執行の合成 broker。paper 単体は固定シード価格でしか動かず forward 検証に
// ならないが、実ブローカーでの検証には実口座と守り(逆指値)の実機実証が要る — その間を埋める経路。
// 安全性: フィードは発注系を持たない port.MarketFeed としてのみ保持するので **実弾が飛ぶ経路が型として
// 存在しない**。config 側でも paper_live_feed は live_config で reject される(紙執行を live と記録しない)。
// 🛑 *Paper は**埋め込み**。紙の帳簿側(建玉/注文/約定/OCO/AdoptOpenPositions)は
// 11 本の手書き転送を置いていたが、全て `return b.paper.X(...)` の 1 行だった。
// 埋め込みにしても外から見える型は変わらない: Paper は QuotesBatched も GetTickers も
// 持たないので batchedQuoteSource の判定は下の明示実装だけが決め、broker に対する
// もう 1 つの型アサーション brk.(paperBook) は AdoptOpenPositions が昇格して同じ結果になる。
// フィード側(GetTicker / GetKlines / RefreshToken)は**必ず override したままにする** —
// 埋め込みに任せると実フィードではなく紙の固定価格を返し、forward 記録が偽物になる。
type PaperLiveFeed struct {
	*Paper
	feed port.MarketFeed
}

func NewPaperLiveFeed(paper *Paper, feed port.MarketFeed) *PaperLiveFeed {
	return &PaperLiveFeed{Paper: paper, feed: feed}
}

// フィード断は paper の最後の価格に落ちず error(古い価格で約定させない)。
func (b *PaperLiveFeed) GetTicker(ctx context.Context, symbol string) (*market.Ticker, error) {
	tk, err := b.feed.GetTicker(ctx, symbol)
	if err != nil {
		return nil, err
	}
	// Stale(前日終値 indicative)は紙の帳簿に入れない(引け前フラット化が実勢でない価格で記録される)。
	if tk != nil && tk.Last > 0 && !tk.Stale {
		b.Paper.SetPrice(symbol, tk.Last)
	}
	return tk, nil
}

func (b *PaperLiveFeed) QuotesBatched() bool { return QuotesBatched(b.feed) }

// 監視 120 銘柄超えの検知は 2 枚のデコレータの内側にあるので素通しする。
func (b *PaperLiveFeed) QuoteChunks() int { return QuoteChunks(b.feed) }

// 日足は実フィード側(paper は履歴を持たない)。
func (b *PaperLiveFeed) GetKlines(ctx context.Context, symbol string, p port.KlinePeriod, n int) ([]market.Candle, error) {
	return b.feed.GetKlines(ctx, symbol, p, n)
}

// 立花のセッションは日次で切れるので実フィード側だけ更新が要る。
func (b *PaperLiveFeed) RefreshToken(ctx context.Context) error {
	if err := b.feed.RefreshToken(ctx); err != nil {
		return err
	}
	return b.Paper.RefreshToken(ctx)
}
