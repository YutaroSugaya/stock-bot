package broker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// fakeFeed records every call so the tests can assert HOW MANY requests the
// decorator turned N GetTicker calls into.
type fakeFeed struct {
	mu        sync.Mutex
	now       time.Time
	prices    map[string]float64
	batches   [][]string // one entry per upstream REQUEST (chunk), not per call
	singles   []string   // one entry per GetTicker call
	batchErr  error
	calls     int // 上流呼び出し回数(エラー時も数える)
	batchSize int // 1 リクエストの銘柄上限(0 = 無制限 = 常に 1 リクエスト)
}

// QuoteBatchSize は上流の申告。0 のときは実装しないのと同じ扱い(= 分割なし)。
func (f *fakeFeed) QuoteBatchSize() int { return f.batchSize }

func splitSyms(syms []string, size int) [][]string {
	if size <= 0 || len(syms) <= size {
		return [][]string{append([]string(nil), syms...)}
	}
	var out [][]string
	for start := 0; start < len(syms); start += size {
		end := start + size
		if end > len(syms) {
			end = len(syms)
		}
		out = append(out, append([]string(nil), syms[start:end]...))
	}
	return out
}

func newFakeFeed(prices map[string]float64) *fakeFeed {
	return &fakeFeed{now: time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC), prices: prices}
}

func (f *fakeFeed) clock() func() time.Time {
	return func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
}

func (f *fakeFeed) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakeFeed) GetTicker(_ context.Context, s string) (*market.Ticker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.singles = append(f.singles, s)
	p, ok := f.prices[s]
	if !ok {
		return nil, fmt.Errorf("no quote for %s", s)
	}
	return &market.Ticker{Symbol: s, Last: p, Bid: p, Ask: p, Timestamp: f.now}, nil
}

// batchSize は上流が 1 リクエストに詰められる銘柄数(0 = 無制限)。立花の 120 と同じく
// 超過分は**別リクエスト**になるので、fake も分割して数える — ここを 1 件と数えると
// 「120 を超えた瞬間に通信量が倍になる」というテスト対象の現象そのものが見えなくなる。
func (f *fakeFeed) GetTickers(_ context.Context, syms []string) (map[string]*market.Ticker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	f.batches = append(f.batches, splitSyms(syms, f.batchSize)...)
	out := map[string]*market.Ticker{}
	for _, s := range syms {
		if p, ok := f.prices[s]; ok {
			out[s] = &market.Ticker{Symbol: s, Last: p, Bid: p, Ask: p, Timestamp: f.now}
		}
	}
	return out, nil
}

func (f *fakeFeed) GetKlines(context.Context, string, port.KlinePeriod, int) ([]market.Candle, error) {
	return nil, nil
}
func (f *fakeFeed) RefreshToken(context.Context) error { return nil }

func (f *fakeFeed) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

// 定常状態だけを測るための仕切り直し(初回登録は 1 銘柄ずつ全量取得が走るので、
// 混ぜると起動時の一過性コストが定常の通信量を覆い隠す)。
func (f *fakeFeed) upstreamCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeFeed) resetBatches() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = nil
}

// これが目的そのもの: N 銘柄ぶんの GetTicker が **1 リクエスト**に畳まれること。
func TestBatchQuoteFeedCollapsesManyGetTickersIntoOneRequest(t *testing.T) {
	f := newFakeFeed(map[string]float64{"7203": 2900, "6758": 3100, "9984": 8800})
	bf := NewBatchQuoteFeed(f, time.Second, f.clock())
	ctx := context.Background()

	// 1周目: 各銘柄は初回なので登録されつつ取得される。
	for _, s := range []string{"7203", "6758", "9984"} {
		if _, err := bf.GetTicker(ctx, s); err != nil {
			t.Fatalf("warmup %s: %v", s, err)
		}
	}
	warm := f.batchCount()

	// 2周目: キャッシュが新鮮なうちは 1 本も増えない。
	for _, s := range []string{"7203", "6758", "9984"} {
		tk, err := bf.GetTicker(ctx, s)
		if err != nil {
			t.Fatalf("cached %s: %v", s, err)
		}
		if tk.Symbol != s {
			t.Fatalf("銘柄取り違え: want %s got %s", s, tk.Symbol)
		}
	}
	if f.batchCount() != warm {
		t.Fatalf("キャッシュが効いていない: %d → %d", warm, f.batchCount())
	}

	// 3周目: maxAge 経過後は **1 回だけ**まとめて取り直す(銘柄ごとではない)。
	f.advance(2 * time.Second)
	for _, s := range []string{"7203", "6758", "9984"} {
		if _, err := bf.GetTicker(ctx, s); err != nil {
			t.Fatalf("refetch %s: %v", s, err)
		}
	}
	if got := f.batchCount() - warm; got != 1 {
		t.Fatalf("3銘柄で %d リクエスト — 1 回に畳まれるべき", got)
	}
	last := f.batches[len(f.batches)-1]
	if len(last) != 3 {
		t.Fatalf("まとめて取っていない: %v", last)
	}
}

// 古い価格を「新鮮なふり」で返さない。フィードが落ちたら error にする
// (paper_live_feed の契約: 古い価格で約定させない)。
func TestBatchQuoteFeedNeverServesStalePriceAsFresh(t *testing.T) {
	f := newFakeFeed(map[string]float64{"7203": 2900})
	bf := NewBatchQuoteFeed(f, time.Second, f.clock())
	ctx := context.Background()

	if _, err := bf.GetTicker(ctx, "7203"); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	f.mu.Lock()
	f.batchErr = errors.New("feed down")
	f.mu.Unlock()
	f.advance(10 * time.Second)

	if tk, err := bf.GetTicker(ctx, "7203"); err == nil {
		t.Fatalf("フィード断なのに価格を返した: %+v", tk)
	}
}

// 応答から落ちた銘柄(売買停止等)は error。他銘柄の価格で埋めない。
func TestBatchQuoteFeedErrorsForSymbolsMissingFromTheResponse(t *testing.T) {
	f := newFakeFeed(map[string]float64{"7203": 2900})
	bf := NewBatchQuoteFeed(f, time.Second, f.clock())
	ctx := context.Background()

	if _, err := bf.GetTicker(ctx, "9999"); err == nil {
		t.Fatal("応答に無い銘柄に価格が付いた")
	}
	tk, err := bf.GetTicker(ctx, "7203")
	if err != nil || tk.Last != 2900 {
		t.Fatalf("正常な銘柄まで巻き添えになった: %+v %v", tk, err)
	}
}

// 1 リクエストの上限(立花は 120 銘柄)を超えると 1 回の取り直しが 2 リクエストに割れる。
// 取り直しの間隔が固定だと **121 銘柄目で通信量が階段状に倍増する** — 立花から
// 1 日の上限超過を指摘された経路の再発形で、建玉は決済まで積み上がるのでいつか必ず踏む。
// 間隔をチャンク数だけ延ばし、**1日の総リクエスト数を監視銘柄数から切り離す**。
// 代償は 120 超えのときの鮮度低下だが、黙って通信量が倍になるよりは観測できる形で劣化させる。
func TestBatchQuoteFeedKeepsRequestCountIndependentOfWatchSize(t *testing.T) {
	const (
		base   = 3 * time.Second
		window = 2 * time.Minute
		step   = 200 * time.Millisecond
	)
	requests := func(n int) int {
		prices := map[string]float64{}
		syms := make([]string, 0, n)
		for i := 0; i < n; i++ {
			s := fmt.Sprintf("%d", 1000+i)
			prices[s] = 1000
			syms = append(syms, s)
		}
		f := newFakeFeed(prices)
		f.batchSize = 120
		bf := NewBatchQuoteFeed(f, base, f.clock())
		ctx := context.Background()
		// 初回登録は「新しい銘柄が来るたびに全量取り直し」で銘柄数ぶんかかる。これは
		// プロセス起動時に1回きりのコストなので、定常の通信量とは分けて測る。
		for _, s := range syms {
			if _, err := bf.GetTicker(ctx, s); err != nil {
				t.Fatalf("warmup n=%d %s: %v", n, s, err)
			}
		}
		f.resetBatches()
		for elapsed := time.Duration(0); elapsed < window; elapsed += step {
			for _, s := range syms {
				if _, err := bf.GetTicker(ctx, s); err != nil {
					t.Fatalf("n=%d %s: %v", n, s, err)
				}
			}
			f.advance(step)
		}
		return f.batchCount()
	}
	// 40銘柄 = 1チャンク、240銘柄 = 2チャンク。刻み幅による誤差があるので同値ではなく
	// 「増えない」で判定する(修正前は 2 倍になるので余裕で落ちる)。
	small, large := requests(40), requests(240)
	if large > small*11/10 {
		t.Errorf("監視 40銘柄=%d リクエスト → 240銘柄=%d リクエスト。銘柄数で通信量が増えてはいけない", small, large)
	}
}

// 監視から外れた銘柄は batch から落ちる(いつまでも引き続けない)。
func TestBatchQuoteFeedForgetsSymbolsNobodyAsksFor(t *testing.T) {
	f := newFakeFeed(map[string]float64{"7203": 2900, "6758": 3100})
	bf := NewBatchQuoteFeed(f, time.Second, f.clock())
	ctx := context.Background()

	for _, s := range []string{"7203", "6758"} {
		if _, err := bf.GetTicker(ctx, s); err != nil {
			t.Fatalf("warmup %s: %v", s, err)
		}
	}
	// 6758 を誰も要求しないまま忘却期間を跨ぐ。
	for i := 0; i < 3; i++ {
		f.advance(quoteForgetFactor * time.Second)
		if _, err := bf.GetTicker(ctx, "7203"); err != nil {
			t.Fatalf("keepalive: %v", err)
		}
	}
	last := f.batches[len(f.batches)-1]
	for _, s := range last {
		if s == "6758" {
			t.Fatalf("監視外の銘柄を引き続けている: %v", last)
		}
	}
}

// 一括に対応しないフィード(paper 単体など)は素通し。
func TestBatchQuoteFeedPassesThroughNonBatchFeeds(t *testing.T) {
	plain := plainFeed{}
	if got := NewBatchQuoteFeed(plain, time.Second, nil); got != port.MarketFeed(plain) {
		t.Fatal("一括非対応のフィードは包まずそのまま返すべき")
	}
}

type plainFeed struct{}

func (plainFeed) GetTicker(context.Context, string) (*market.Ticker, error) { return nil, nil }
func (plainFeed) GetKlines(context.Context, string, port.KlinePeriod, int) ([]market.Candle, error) {
	return nil, nil
}
func (plainFeed) RefreshToken(context.Context) error { return nil }

// 同時に走る多数の GetTicker が **1 回**の取得に合流すること(38 本の
// goroutine が各自 fetch すると一括の意味が消える)。
func TestBatchQuoteFeedCoalescesConcurrentCallers(t *testing.T) {
	prices := map[string]float64{}
	syms := []string{}
	for i := 0; i < 30; i++ {
		s := fmt.Sprintf("%04d", i)
		syms = append(syms, s)
		prices[s] = 100
	}
	f := newFakeFeed(prices)
	bf := NewBatchQuoteFeed(f, time.Second, f.clock())
	ctx := context.Background()

	for _, s := range syms { // 登録 + 初回取得
		if _, err := bf.GetTicker(ctx, s); err != nil {
			t.Fatalf("warmup %s: %v", s, err)
		}
	}
	warm := f.batchCount()
	f.advance(2 * time.Second)

	var wg sync.WaitGroup
	for _, s := range syms {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			if _, err := bf.GetTicker(ctx, s); err != nil {
				t.Errorf("concurrent %s: %v", s, err)
			}
		}(s)
	}
	wg.Wait()

	if got := f.batchCount() - warm; got != 1 {
		t.Fatalf("同時 %d 呼び出しが %d リクエストになった — 1 回に合流すべき", len(syms), got)
	}
}

// 🛑 上流が落ちている間、**取り直しの間隔を守ること**。
//
// エラー時にキャッシュを触らない設計は「古い価格を新鮮なふりで返さない」ためだが、
// 副作用として**全 bundle が各自 fetch をやり直す**。実測: 41銘柄・3秒間隔で
// 正常時 60リクエスト/60秒 に対し、**障害時 23,370リクエスト/60秒**(390倍)。
// 本番はレートリミッタ(2 req/s)で頭打ちになるが、それは
//
//	① 場中ずっと 2 req/s = 39,600回/日(立花の上限 10,000 の4倍)
//	② **発注リクエストがこの再取得の行列の後ろで待たされる**(リミッタに優先度が無い)
//
// を意味する。②の方が重い — 障害中こそ決済を通したい。
//
// 直し方は「エラーもキャッシュする」ではなく「**エラーを次の間隔まで覚えておく**」。
// 呼び手には引き続き error を返す(古い価格は決して返さない)。
func TestBatchQuoteFeedDoesNotStormUpstreamWhileItIsDown(t *testing.T) {
	const (
		base   = 3 * time.Second
		window = 60 * time.Second
		step   = 100 * time.Millisecond
	)
	prices := map[string]float64{}
	syms := []string{}
	for i := 0; i < 41; i++ {
		s := fmt.Sprintf("%d", 1000+i)
		prices[s] = 1000
		syms = append(syms, s)
	}
	f := newFakeFeed(prices)
	f.batchSize = 120
	bf := NewBatchQuoteFeed(f, base, f.clock())
	ctx := context.Background()
	for _, s := range syms {
		if _, err := bf.GetTicker(ctx, s); err != nil {
			t.Fatalf("warmup %s: %v", s, err)
		}
	}

	f.mu.Lock()
	f.batchErr = errors.New("upstream down")
	f.mu.Unlock()
	// 障害直後の maxAge ぶんは**キャッシュが正当に新鮮**なので価格を返してよい。
	// 測るのはその窓を過ぎてからの挙動。
	f.advance(2 * base)
	f.mu.Lock()
	f.calls = 0
	f.mu.Unlock()

	served := 0
	for e := time.Duration(0); e < window; e += step {
		for _, s := range syms {
			if _, err := bf.GetTicker(ctx, s); err == nil {
				served++
			}
		}
		f.advance(step)
	}

	// 60秒 ÷ 3秒 = 20 回程度。刻み幅ぶんの誤差を見て 25 を上限にする。
	if got := f.upstreamCalls(); got > 25 {
		t.Errorf("障害中の上流呼び出し %d 回/60秒 — 間隔(3秒)を守れば 20回前後のはず", got)
	}
	// **古い価格を返し始めていないこと**(こちらの方が重い不変条件)。
	if served != 0 {
		t.Errorf("障害中に %d 件の価格を返した — 古い価格で紙約定が毒される", served)
	}
}
