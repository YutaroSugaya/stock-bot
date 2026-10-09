package app

import (
	"context"
	"sync"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// dashboard 用の読み取り専用ランキング。2秒ポーリングでユニバース全体を毎回
// 再ランクしないよう短い TTL でキャッシュする(日足は引けでしか動かない)。
type ScanProvider struct {
	candles   port.CandleRepository
	symbols   []string
	screeners []strategy.Screener
	ttl       time.Duration
	clock     clock.Clock

	mu     sync.Mutex
	cached []strategy.Candidate
	at     time.Time
}

// screeners は表示用の全候補戦略。Selector が arm できる集合とは独立。
func NewScanProvider(candles port.CandleRepository, symbols []string, screeners []strategy.Screener, ttl time.Duration, c clock.Clock) *ScanProvider {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if c == nil {
		c = clock.System()
	}
	return &ScanProvider{candles: candles, symbols: symbols, screeners: screeners, ttl: ttl, clock: c}
}

func (p *ScanProvider) Ranking(ctx context.Context) []strategy.Candidate {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	if p.cached != nil && now.Sub(p.at) < p.ttl {
		return p.cached
	}
	universe := make(map[string][]market.Candle, len(p.symbols))
	for _, sym := range p.symbols {
		if cs, err := p.candles.List(ctx, sym, port.PeriodDaily, selectorCandleLookback); err == nil && len(cs) >= 26 {
			universe[sym] = cs
		}
	}
	p.cached = strategy.RankCandidates(universe, p.screeners)
	p.at = now
	return p.cached
}
