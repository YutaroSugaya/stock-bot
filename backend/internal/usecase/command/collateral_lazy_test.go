package command

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// countingBroker counts the 口座余力照会 so a test can assert the wire cost of a
// rejected entry. 立花では GetAccountMargin 1 回 = wire 3 リクエスト
// (買付余力 + 建余力 + 保証金率) なので、ここが 1 増えるたびに実際は 3 回飛ぶ。
type countingBroker struct {
	*broker.Paper
	marginCalls atomic.Int64
}

func (b *countingBroker) GetAccountMargin(ctx context.Context) (*order.AccountMargin, error) {
	b.marginCalls.Add(1)
	return b.Paper.GetAccountMargin(ctx)
}

// 構造ゲート(建玉枠・ナンピン禁止など repo だけで判定できるもの)で reject される
// エントリーは、broker の余力照会を **1 回も** 払ってはならない。
//
// 実測: live 枠が満杯(account_max_open_positions=2)の状態で armed 銘柄が
// 3 秒ごとにシグナルを出し続け、毎回 余力照会 3 本を払ってから枠オーバーで捨てていた。
// 1,680回/時 = その日の立花 API の上限(1 万回)を単独で焼き切る量。
func TestTradingCycle_StructuralRejectCostsNoMarginQuery(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	cb := &countingBroker{Paper: h.broker}
	// SnapshotBuilder だけ差し替える(執行経路は harness のまま)。
	caps := SnapshotCaps{
		MaxDailyLossJPY: 5000, MaxConsecutiveLosses: 4, PerSymbolMaxOpenPositions: 1,
		AccountMaxOpenPositions: 1, AccountMaxDailyLossJPY: 18000, RequiredMarginRate: 0.30, WindowMinutes: 60,
	}
	c := clock.Fixed(now)
	sb := NewSnapshotBuilder(h.posRepo, h.tradeRepo, cb, h.emergency, h.hours, c, caps)
	h.cycle.snapshot = sb

	// 別銘柄で口座枠を埋める → 7203 のエントリーは account_open_positions で reject される。
	if _, err := h.posRepo.Insert(context.Background(), port.PositionInsertInput{
		Symbol: "6758", Side: order.SideBuy, Quantity: 100, EntryPrice: 1000,
		StrategyConfigID: "cfg-other", HoldingMode: order.HoldingMultiday,
	}); err != nil {
		t.Fatalf("seed position: %v", err)
	}

	cfg := multidayConfig()
	in := evalInput(now, cfg, dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	res, err := h.cycle.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Entered {
		t.Fatal("口座枠が満杯なのにエントリーした")
	}
	if got := cb.marginCalls.Load(); got != 0 {
		t.Fatalf("構造ゲートで reject されるエントリーが余力照会を %d 回払った (want 0): reason=%q", got, res.RejectReason)
	}
}

// 逆に、構造ゲートを通ったエントリーは余力照会を **必ず** 払う。ここが 0 になると
// 「通信量を削ったつもりで担保チェックを消した」ことになる。
func TestTradingCycle_PassingEntryStillPaysOneMarginQuery(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	cb := &countingBroker{Paper: h.broker}
	caps := SnapshotCaps{
		MaxDailyLossJPY: 5000, MaxConsecutiveLosses: 4, PerSymbolMaxOpenPositions: 1,
		AccountMaxOpenPositions: 3, AccountMaxDailyLossJPY: 18000, RequiredMarginRate: 0.30, WindowMinutes: 60,
	}
	c := clock.Fixed(now)
	sb := NewSnapshotBuilder(h.posRepo, h.tradeRepo, cb, h.emergency, h.hours, c, caps)
	h.cycle.snapshot = sb

	cfg := multidayConfig()
	in := evalInput(now, cfg, dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	res, err := h.cycle.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Entered {
		t.Fatalf("エントリーが通るはずのケースで reject された: %q", res.RejectReason)
	}
	if got := cb.marginCalls.Load(); got != 1 {
		t.Fatalf("担保チェックの余力照会が %d 回 (want 1)", got)
	}
}

// BuildStructural は broker を触らない。触っていない以上、余力は **不明** であって
// 「充分」ではない — MarginStatusUnknown を立てておくことで、この snapshot が
// 誤って full ゲートに渡っても fail-close 側(margin_status_unavailable)に倒れる。
// 配線を間違えたときに担保チェックが素通りするのを、型ではなくゼロ値の意味で防ぐ。
func TestBuildStructural_LeavesMarginUnknownSoFullGateFailsClose(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	cb := &countingBroker{Paper: h.broker}
	c := clock.Fixed(now)
	sb := NewSnapshotBuilder(h.posRepo, h.tradeRepo, cb, h.emergency, h.hours, c, SnapshotCaps{RequiredMarginRate: 0.30})
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "7203", EntryPrice: 2500, Quantity: 100}

	snap := sb.BuildStructural(context.Background(), "7203", nil, sig, order.ExecCash)
	if cb.marginCalls.Load() != 0 {
		t.Fatal("BuildStructural が broker を叩いた")
	}
	if !snap.MarginStatusUnknown {
		t.Fatal("BuildStructural の snapshot は MarginStatusUnknown=true でなければならない(未照会は fail-close 側)")
	}

	sb.FillCollateral(context.Background(), &snap, sig, order.ExecCash)
	if cb.marginCalls.Load() != 1 {
		t.Fatalf("FillCollateral が余力照会を %d 回 (want 1)", cb.marginCalls.Load())
	}
	if snap.MarginStatusUnknown {
		t.Fatal("照会が成功したのに MarginStatusUnknown が残っている")
	}
}

// 🚨 実測: armed 銘柄が毎ティック entry シグナルを出し、構造ゲートを
// 通ってから `gross_notional_cap`(照会の**後**にしか判定できないゲート)で落ちる
// 状態になると、6 秒ごとに口座照会 3 本が飛んだ。後場だけで口座照会が千回超 = wire 数千回。
// 保証金は **口座単位の量**なので、ティック数ぶん払ってはならない。
func TestFillCollateral_RepeatedTicksPayOneMarginQuery(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	cb := &countingBroker{Paper: h.broker}
	c := clock.Fixed(now)
	sb := NewSnapshotBuilder(h.posRepo, h.tradeRepo, cb, h.emergency, h.hours, c,
		SnapshotCaps{RequiredMarginRate: 0.30})
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "7203", EntryPrice: 2500, Quantity: 100}

	for i := 0; i < 100; i++ {
		snap := sb.BuildStructural(context.Background(), "7203", nil, sig, order.ExecCash)
		sb.FillCollateral(context.Background(), &snap, sig, order.ExecCash)
		if snap.MarginStatusUnknown {
			t.Fatalf("tick %d: 担保値が埋まっていない — キャッシュが値を配れていない", i)
		}
	}
	if got := cb.marginCalls.Load(); got != 1 {
		t.Fatalf("100 ティックで口座照会が %d 回 (want 1)", got)
	}
}

// 約定 / 決済で口座は変わる。無効化されたら次のティックは必ず訊き直す。
func TestFillCollateral_InvalidatedAfterAFillQueriesAgain(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	cb := &countingBroker{Paper: h.broker}
	c := clock.Fixed(now)
	cache := NewAccountMarginCache(cb, time.Hour, c)
	sb := NewSnapshotBuilder(h.posRepo, h.tradeRepo, cb, h.emergency, h.hours, c,
		SnapshotCaps{RequiredMarginRate: 0.30}).WithAccountMargin(cache)
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "7203", EntryPrice: 2500, Quantity: 100}

	fill := func() {
		snap := sb.BuildStructural(context.Background(), "7203", nil, sig, order.ExecCash)
		sb.FillCollateral(context.Background(), &snap, sig, order.ExecCash)
	}
	fill()
	fill()
	cache.Invalidate()
	fill()
	if got := cb.marginCalls.Load(); got != 2 {
		t.Fatalf("無効化を挟んだ 3 ティックで照会が %d 回 (want 2)", got)
	}
}
