package broker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// 銘柄ごとの GetTicker を tick 1リクエストに畳む(立花からの高負荷の指摘への対処)。間隔を伸ばすだけの
// 削減には対価があった — 1分足が捉える価格水準が 3〜4 個から 2 個に落ち、決済判定も最大30秒遅れ、しかも
// 監視銘柄が増えれば呼び出しは再び線形に増える。一括なら呼び出し数が銘柄数から切り離される。
// bundle 側の `GetTicker(symbol)` を残してフィードの**手前**で畳むのは、呼び出し経路を書き換えると
// Stale の扱い・paper への価格反映・決済判定の既存不変条件を全部見直すことになるため。

// この回数 × maxAge 要求が無い銘柄は batch から落とす(決済済み・disarm 済みを引き続けない)。
const quoteForgetFactor = 5

type batchQuoter interface {
	GetTickers(ctx context.Context, symbols []string) (map[string]*market.Ticker, error)
}

// 上流が 1 リクエストに詰められる銘柄数の申告(立花は 120)。申告しない実装は
// 「分割しない」= 1 回の取り直しが常に 1 リクエスト、として扱う。
type quoteBatchSizer interface{ QuoteBatchSize() int }

type batchedQuoteSource interface{ QuotesBatched() bool }

type quoteChunkSource interface{ QuoteChunks() int }

// QuoteChunks は「いま 1 回の取り直しが何リクエストになるか」。デコレータ越しに
// 読むための入口(QuotesBatched と同じ形)。不明なら 1。
func QuoteChunks(b any) int {
	if s, ok := b.(quoteChunkSource); ok {
		if n := s.QuoteChunks(); n > 0 {
			return n
		}
	}
	return 1
}

// 価格ループの間隔がここにぶら下がる: 畳めていない実フィードは必ず throttle 側に落ちる。
func QuotesBatched(b any) bool {
	s, ok := b.(batchedQuoteSource)
	return ok && s.QuotesBatched()
}

func (b *BatchQuoteFeed) QuotesBatched() bool { return true }

type quoteEntry struct {
	tk        *market.Ticker
	fetchedAt time.Time
	err       error // この銘柄が直近の一括応答から落ちていた
}

// BatchQuoteFeed は監視銘柄全部を 1 本のレーンで一括取得する(D-7: 段階ウォッチの別レーンは撤去。
// 回数はレーン数 × 頻度で決まるので、別レーンは通信量を足し算で増やすだけだった)。
type BatchQuoteFeed struct {
	inner  port.MarketFeed
	batch  batchQuoter
	maxAge time.Duration
	clock  clock.Clock

	fetchMu sync.Mutex // 上流 fetch を直列化 = 併合する

	mu     sync.Mutex
	want   map[string]time.Time // symbol -> 最後に要求された時刻
	quotes map[string]quoteEntry
	// 直近の取得失敗。**成功と同じ間隔で覚えておく**ためのもので、価格の代わりでは
	// ない(呼び手には引き続き error を返す)。
	lastFail fetchFailure
}

// fetchFailure は「いつ失敗したか」。エラーをキャッシュするのではなく、
// **次の取り直し時刻まで上流を叩き直さない**ために使う。
type fetchFailure struct {
	err error
	at  time.Time
}

// 🛑 上流が落ちている間に取り直しの間隔が消えるのを防ぐ。
//
// エラー時にキャッシュを触らない設計(古い価格を新鮮なふりで返さない)の副作用として、
// **全 bundle が各自 fetch をやり直す**。実測: 41銘柄・3秒間隔で
// 正常時 60リクエスト/60秒 → 障害時 **24,600リクエスト/60秒**。本番はレートリミッタ
// (2 req/s)が頭打ちにするが、それは ①場中ずっと 2 req/s = 39,600回/日(立花の上限の4倍)
// ②**発注リクエストがこの再取得の行列の後ろで待たされる**(リミッタに優先度が無い)
// を意味する。②の方が重い — 障害中こそ決済を通したい。
func (f fetchFailure) suppresses(now time.Time, maxAge time.Duration) bool {
	return f.err != nil && now.Sub(f.at) <= maxAge
}

// maxAge は価格ループ間隔に合わせる。畳めない inner はそのまま返す(paper 単体など)。
func NewBatchQuoteFeed(inner port.MarketFeed, maxAge time.Duration, c clock.Clock) port.MarketFeed {
	bq, ok := inner.(batchQuoter)
	if !ok {
		return inner
	}
	if c == nil {
		c = clock.System()
	}
	if maxAge <= 0 {
		maxAge = time.Second
	}
	return &BatchQuoteFeed{
		inner: inner, batch: bq, maxAge: maxAge, clock: c,
		want: map[string]time.Time{}, quotes: map[string]quoteEntry{},
	}
}

// chunksFor は n 銘柄の取り直しに要する上流リクエスト数。
func (b *BatchQuoteFeed) chunksFor(n int) int {
	if n <= 0 {
		return 1
	}
	size := 0
	if s, ok := b.batch.(quoteBatchSizer); ok {
		size = s.QuoteBatchSize()
	}
	if size <= 0 {
		return 1 // 上限の申告が無い = 何銘柄でも 1 リクエスト
	}
	return (n + size - 1) / size
}

// QuoteChunks は現在の監視集合での 1 回ぶんのリクエスト数(観測・警告用)。
func (b *BatchQuoteFeed) QuoteChunks() int {
	b.mu.Lock()
	n := len(b.want)
	b.mu.Unlock()
	return b.chunksFor(n)
}

// 🛑 取り直しの間隔は**チャンク数に比例して伸ばす**。上限 120 を超えた瞬間に
// 1 回が 2 リクエストに割れるので、間隔が固定だと通信量が階段状に倍増する
// (立花から 1 日の上限超過を指摘された経路の再発形。建玉は決済まで積み上がるので
// いつか必ず踏む)。間隔 × チャンク数 を一定に保てば、**1日の総リクエスト数が
// 監視銘柄数から完全に切り離される**。代償は 120 超えのときの鮮度低下だが、
// 黙って通信量が倍になるより、観測できる形で解像度を落とすほうを選ぶ。
func (b *BatchQuoteFeed) effectiveMaxAge(base time.Duration, n int) time.Duration {
	return base * time.Duration(b.chunksFor(n))
}

func (b *BatchQuoteFeed) GetTicker(ctx context.Context, symbol string) (*market.Ticker, error) {
	now := b.clock()
	b.mu.Lock()
	b.want[symbol] = now
	maxAge := b.effectiveMaxAge(b.maxAge, len(b.want))
	e, hit := b.quotes[symbol]
	fresh := hit && now.Sub(e.fetchedAt) <= maxAge
	b.mu.Unlock()

	if fresh {
		return serve(symbol, e)
	}

	// 併合: 最初の1本だけが実際に引き、残りは fetchMu 下の再確認で更新済みを見る。
	b.fetchMu.Lock()
	defer b.fetchMu.Unlock()

	now = b.clock()
	b.mu.Lock()
	e, hit = b.quotes[symbol]
	fresh = hit && now.Sub(e.fetchedAt) <= maxAge
	// 直近の失敗が「まだ次の取り直し時刻に達していない」なら上流を叩かずに返す。
	fail := b.lastFail
	suppressed := !fresh && fail.suppresses(now, maxAge)
	b.mu.Unlock()
	if fresh {
		return serve(symbol, e)
	}
	if suppressed {
		return nil, fail.err
	}

	if err := b.refresh(ctx, now); err != nil {
		return nil, err
	}

	b.mu.Lock()
	e, hit = b.quotes[symbol]
	b.mu.Unlock()
	if !hit {
		return nil, fmt.Errorf("tachibana: no market price for %q", symbol)
	}
	return serve(symbol, e)
}

// エラー時はキャッシュを触らず伝播する(古い価格を新鮮なふりで返さない)。
func (b *BatchQuoteFeed) refresh(ctx context.Context, now time.Time) error {
	b.mu.Lock()
	// 忘却も実効間隔を基準にする(間隔だけ伸ばして忘却を据え置くと、監視 120 超えの
	// ときに「まだ現役の銘柄が刈られて次の tick で登録し直される」振動になる)。
	cutoff := now.Add(-quoteForgetFactor * b.effectiveMaxAge(b.maxAge, len(b.want)))
	syms := make([]string, 0, len(b.want))
	for s, last := range b.want {
		if last.Before(cutoff) {
			delete(b.want, s) // 監視から外れた銘柄は刈る
			delete(b.quotes, s)
			continue
		}
		syms = append(syms, s)
	}
	b.mu.Unlock()

	if len(syms) == 0 {
		return nil
	}
	got, err := b.batch.GetTickers(ctx, syms)
	if err != nil {
		b.mu.Lock()
		b.lastFail = fetchFailure{err: err, at: now}
		b.mu.Unlock()
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastFail = fetchFailure{}
	for _, s := range syms {
		if tk, ok := got[s]; ok {
			b.quotes[s] = quoteEntry{tk: tk, fetchedAt: now}
			continue
		}
		// 応答から落ちた銘柄は「取れなかった」を記録する(他銘柄の価格で埋めない)。
		b.quotes[s] = quoteEntry{fetchedAt: now, err: fmt.Errorf("tachibana: no market price for %q", s)}
	}
	return nil
}

func serve(symbol string, e quoteEntry) (*market.Ticker, error) {
	if e.err != nil {
		return nil, e.err
	}
	if e.tk == nil {
		return nil, fmt.Errorf("tachibana: no market price for %q", symbol)
	}
	cp := *e.tk // 共有エントリを呼び手に書き換えさせないための複製
	return &cp, nil
}

func (b *BatchQuoteFeed) GetKlines(ctx context.Context, symbol string, p port.KlinePeriod, n int) ([]market.Candle, error) {
	return b.inner.GetKlines(ctx, symbol, p, n)
}

func (b *BatchQuoteFeed) RefreshToken(ctx context.Context) error { return b.inner.RefreshToken(ctx) }
