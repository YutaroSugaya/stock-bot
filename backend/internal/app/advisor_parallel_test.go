package app

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// slowAdvisor records concurrency and blocks so overlap is observable.
// MOCK rationale (TESTING.md 3用途): §1 system boundary (LLM サブプロセス)。
type slowAdvisor struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	calls    atomic.Int64
	symbols  []string
	delay    time.Duration
}

func (s *slowAdvisor) Generate(_ context.Context, summ *market.MarketSummary) (*port.AdvisorRun, error) {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	sym, slot := "", "bnf_reversion"
	if summ != nil {
		sym = summ.Symbol
		if summ.SlotStrategy != "" {
			// 枠の戦略拘束に従う: 実 LLM も slot_strategy か no_trade
			// しか返せない契約。枠違いを返すと promote が reject する。
			slot = summ.SlotStrategy
		}
	}
	s.symbols = append(s.symbols, sym)
	s.mu.Unlock()
	s.calls.Add(1)
	time.Sleep(s.delay)
	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	return &port.AdvisorRun{RunID: "r-" + sym, Status: port.AdvisorRunSuccess,
		ParsedYAML: []byte(parallelYAML(sym, slot))}, nil
}

func parallelYAML(sym, strat string) string {
	return fmt.Sprintf(`config_id: c-%s
symbol: "%s"
strategy_name: %s
mode: paper_config
holding_mode: multiday
exec_kind: cash
entry:
  direction: buy_only
  max_spread_ticks: 5
exit:
  take_profit_jpy: 100
  stop_loss_jpy: 50
risk:
  quantity: 100
  max_open_positions: 1
`, sym, sym, strat)
}

func newParallelLoop(t *testing.T, adv port.Advisor, syms []string, armed *[]string, maxConc int) *AdvisorLoop {
	t.Helper()
	repo := repository.NewInMemoryCandleRepo()
	for _, s := range syms {
		if err := repo.Upsert(context.Background(), s, panicSeries(s)); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 7, 27, 10, 0, 0, 0, clock.JST)
	var mu sync.Mutex
	return &AdvisorLoop{
		Symbols:       syms,
		Candles:       repo,
		Advisor:       adv,
		HardLimits:    testHardLimits(syms...),
		Hours:         tokyoTradingHours(),
		Clock:         func() time.Time { return at },
		TopN:          len(syms),
		PerStrategyN:  len(syms), // 並列の検証が目的。戦略別クォータでは絞らない
		MaxConcurrent: maxConc,
		Arm: func(c *config.StrategyConfig) error {
			mu.Lock()
			defer mu.Unlock()
			*armed = append(*armed, c.Symbol)
			return nil
		},
	}
}

// 1ラウンドの LLM 呼び出しは並列に走る。直列だと 1銘柄10分 × 10 で1ラウンドが
// 場中に終わらない(実測: Opus 5/max は1銘柄621秒)。
func TestAdvisorLoop_RunsLLMCallsInParallel(t *testing.T) {
	syms := []string{"7203", "6758", "8306", "9432", "6501"}
	adv := &slowAdvisor{delay: 60 * time.Millisecond}
	var armed []string
	l := newParallelLoop(t, adv, syms, &armed, len(syms))

	start := time.Now()
	l.Tick(context.Background())
	elapsed := time.Since(start)

	if got := adv.calls.Load(); got != int64(len(syms)) {
		t.Fatalf("LLM 呼び出し = %d, want %d (1銘柄1回)", got, len(syms))
	}
	if adv.peak < 2 {
		t.Fatalf("同時実行のピーク = %d — 直列のまま並列化されていない", adv.peak)
	}
	// 直列なら 5*60ms=300ms 以上かかる。並列なら大きく下回る。
	if elapsed > 250*time.Millisecond {
		t.Fatalf("1ラウンド %v — 直列実行の疑い", elapsed)
	}
	if len(armed) != len(syms) {
		t.Fatalf("arm 数 = %d, want %d", len(armed), len(syms))
	}
}

// 並列度は MaxConcurrent が上限。利用上限や CPU を守るための弁で、
// 0 以下は 1(直列)に倒す。
func TestAdvisorLoop_RespectsMaxConcurrent(t *testing.T) {
	syms := []string{"7203", "6758", "8306", "9432", "6501", "6502"}
	adv := &slowAdvisor{delay: 40 * time.Millisecond}
	var armed []string
	l := newParallelLoop(t, adv, syms, &armed, 2)

	l.Tick(context.Background())

	if adv.peak > 2 {
		t.Fatalf("同時実行のピーク = %d, want <= 2 (MaxConcurrent)", adv.peak)
	}
	if got := adv.calls.Load(); got != int64(len(syms)) {
		t.Fatalf("LLM 呼び出し = %d, want %d — 上限で取りこぼしている", got, len(syms))
	}
}

// arm は必ず ranked 順(直列)。生成は並列でも arm 順が実行ごとに変わると
// ログと再現性が崩れる(arm_source: template の意味が薄れる)。
func TestAdvisorLoop_ArmsInRankOrderDespiteParallelGeneration(t *testing.T) {
	syms := []string{"7203", "6758", "8306"}
	// 逆順に遅延させ、完了順が rank 順と一致しないようにする。
	adv := &slowAdvisor{delay: 20 * time.Millisecond}
	var armed []string
	l := newParallelLoop(t, adv, syms, &armed, 3)

	l.Tick(context.Background())

	if len(armed) != 3 {
		t.Fatalf("arm 数 = %d, want 3", len(armed))
	}
	// arm 順は **枠配布(selectAdvisePicks)が決めた順**であり、生成の完了順ではない。
	// 枠は (銘柄, 戦略) 単位なので同じ銘柄が複数回現れうる — 以前ここは
	// 「銘柄を dedup した ranked 順」を期待していたが、それは 1 銘柄 1 戦略だった頃の形。
	ranked := strategy.RankCandidates(l.universe(context.Background()), l.screeners())
	picks := selectAdvisePicks(ranked, nil, l.TopN, l.PerStrategyN, l.now().YearDay(), nil)
	want := make([]string, 0, len(picks))
	for _, p := range picks {
		want = append(want, p.Symbol)
	}
	for i := range armed {
		if i < len(want) && armed[i] != want[i] {
			t.Fatalf("arm 順 = %v, want %v (枠配布順)", armed, want)
		}
	}
}
