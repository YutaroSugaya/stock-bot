package command

import (
	"context"
	"sync"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
	"stockbot/backend/internal/testutil"
)

// barrierBroker は発注を「2 本そろうまで(または window が過ぎるまで)」止める。
// MOCK rationale (TESTING.md 3用途): §1 system boundary — 2 銘柄の発注が broker に
// 同時に届く瞬間を決定論的に作る。排他が無ければ 2 本とも snapshot を取り終えてから
// ここに届く = 同じ空き枠を 2 本が見た状態。
type barrierBroker struct {
	port.LiveBroker
	mu      sync.Mutex
	arrived int
	both    chan struct{}
	window  time.Duration
}

func (b *barrierBroker) PlaceOrder(ctx context.Context, req order.PlaceOrderRequest) (*port.PlaceOrderResult, error) {
	b.mu.Lock()
	b.arrived++
	if b.arrived == 2 {
		close(b.both)
	}
	b.mu.Unlock()
	select {
	case <-b.both:
	case <-time.After(b.window):
	}
	return b.LiveBroker.PlaceOrder(ctx, req)
}

// raceTwoSymbols は口座の枠が残り 1 本のところへ 2 銘柄が同時に entry する。
func raceTwoSymbols(t *testing.T, lock *EntrySerializer) int {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	brk := &barrierBroker{LiveBroker: pb, both: make(chan struct{}), window: 300 * time.Millisecond}
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	pending := safety.NewPendingPositions()
	es := safety.NewEmergencyStop(testutil.TempFlagPath(t), nil)
	caps := SnapshotCaps{
		MaxDailyLossJPY: 5000, MaxConsecutiveLosses: 4, PerSymbolMaxOpenPositions: 1,
		AccountMaxOpenPositions: 1, AccountMaxDailyLossJPY: 18000, RequiredMarginRate: 0.30, WindowMinutes: 60,
	}
	execKindFor := func(order.HoldingMode) order.ExecKind { return order.ExecMarginGeneral }

	var wg sync.WaitGroup
	for _, sym := range []string{"7203", "6758"} {
		eng := strategy.NewEngine(func() string { return "sig-" + sym }, strategy.TimeSeriesMomentum{})
		snap := NewSnapshotBuilder(posRepo, tradeRepo, pb, es, tokyoHours(), c, caps)
		exec := NewExecuteOrder(brk, posRepo, pending, es, c)
		cycle := NewTradingCycle(eng, snap, exec, 100, execKindFor, tokyoHours())
		cycle.EntryLock = lock
		cfg := multidayConfig()
		cfg.ConfigID, cfg.Symbol = "cfg-"+sym, sym
		daily := dailyUptrend(250)
		last := daily[len(daily)-1].Close
		pb.SetPrice(sym, last)
		in := strategy.EvalInput{Now: now, Config: cfg, CandlesDaily: daily,
			Summary: &market.MarketSummary{Symbol: sym, TickSize: market.TickSize(last),
				CurrentRate: market.CurrentRate{Last: last, SpreadTicks: 1}}}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cycle.Execute(ctx, in); err != nil {
				t.Errorf("%s: %v", sym, err)
			}
		}()
	}
	wg.Wait()
	open, err := posRepo.ListOpenAllSymbols(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return len(open)
}

// 足場の検定: 排他が無ければ本当に 2 本建つ(下のテストが空振りで緑にならないことの保証)。
func TestEntryRace_ReproducesWithoutTheLock(t *testing.T) {
	if n := raceTwoSymbols(t, nil); n != 2 {
		t.Fatalf("排他なしで %d 本 — 足場が競合を再現していない(want 2)", n)
	}
}

// 🚨 **口座の最後の 1 枠を 2 銘柄が同時に取りに来たら、建つのは 1 本だけ**。
// 上限の判定は snapshot の件数で、評価は銘柄ごとの goroutine から走る。口座単位の排他が
// 無いと、2 本が同じ空き枠を見て両方建てる(TOCTOU)。live の同時保有枠は 1 とは
// 限らないので、枠 1 のときだけの話ではない。
func TestEntrySerializer_OnlyOneTakesTheLastSlot(t *testing.T) {
	if n := raceTwoSymbols(t, NewEntrySerializer(5*time.Second)); n != 1 {
		t.Fatalf("口座の枠 1 に %d 本建った(want 1)", n)
	}
}

// 🛑 排他を取れないまま待ち続けない。entry の往復(立花は約定待ちで最大 15 秒)が
// 長引いても、他銘柄の price goroutine(= その銘柄の決済判定)を無期限に止めない。
func TestEntrySerializer_GivesUpAfterTheWait(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	rej := repository.NewInMemoryRejectionRepo()
	h.cycle.Rejections = rej
	lock := NewEntrySerializer(20 * time.Millisecond)
	h.cycle.EntryLock = lock
	release, ok := lock.acquire(context.Background())
	if !ok {
		t.Fatal("空きの排他が取れない")
	}
	defer release()

	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)
	res, err := h.cycle.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Entered || res.RejectReason != ReasonEntryLockTimeout {
		t.Fatalf("排他待ちの時間切れで断っていない: entered=%v reason=%q", res.Entered, res.RejectReason)
	}
	if rows := rej.All(); len(rows) != 1 || rows[0].Reason != ReasonEntryLockTimeout {
		t.Fatalf("理由が signal_rejections に残らない: %+v", rows)
	}
}

// entry を提案しないティック(大半)は排他に触れない — 銘柄数ぶんのティックが直列に並ばない。
func TestEntrySerializer_NoTradeDoesNotTakeTheLock(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	h.cycle.engine = strategy.NewEngine(func() string { return "s" }, costFloorStrategy{reason: "no_setup"})
	lock := NewEntrySerializer(20 * time.Millisecond)
	h.cycle.EntryLock = lock
	release, _ := lock.acquire(context.Background())
	defer release()

	cfg := multidayConfig()
	cfg.StrategyName = config.StrategyTimeSeriesMomentum
	res, _ := h.cycle.Execute(context.Background(), evalInput(now, cfg, dailyUptrend(250)))
	if res.RejectReason == ReasonEntryLockTimeout {
		t.Fatal("entry を提案しないティックが排他を待った")
	}
}
