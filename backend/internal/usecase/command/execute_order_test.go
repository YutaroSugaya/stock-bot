package command

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
)

// failingOCOBroker forces the protective-OCO step to fail; failClose additionally
// fails the compensating close.
// MOCK rationale (TESTING.md 3用途): §2 failure injection at the broker boundary.
type failingOCOBroker struct {
	port.LiveBroker
	failClose bool
}

func (f *failingOCOBroker) PlaceSettleOCO(context.Context, port.OCOCloseOrderInput) (string, error) {
	return "", errors.New("oco unavailable")
}

func (f *failingOCOBroker) ClosePosition(ctx context.Context, req port.CloseRequest) (*port.CloseResult, error) {
	if f.failClose {
		return nil, errors.New("close rejected")
	}
	return f.LiveBroker.ClosePosition(ctx, req)
}

// unconfirmedSettleOCOBroker は「OCO 拒否 → 補償の決済は受理されるが約定は非同期で
// 確認できない」立花の形。受理を約定と読むと台帳に幽霊決済が入る。
// MOCK rationale (TESTING.md 3用途): §2 failure injection at the broker boundary.
type unconfirmedSettleOCOBroker struct{ port.LiveBroker }

func (b *unconfirmedSettleOCOBroker) PlaceSettleOCO(context.Context, port.OCOCloseOrderInput) (string, error) {
	return "", errors.New("oco unavailable")
}

func (b *unconfirmedSettleOCOBroker) SettleFillsAsync() bool { return true }

func (b *unconfirmedSettleOCOBroker) ClosePosition(context.Context, port.CloseRequest) (*port.CloseResult, error) {
	// 受理はするが約定値を返さない・注文番号も無い = 解決不能(立花の accepted() は
	// sOrderNumber を見ないので実際に到達しうる形)。
	return &port.CloseResult{Accepted: true}, nil
}

// failingResolveBroker accepts the entry but cannot CONFIRM the fill — e.g. a
// transient API failure after a MARKET order. closeFails additionally fails the
// compensating close.
type failingResolveBroker struct {
	port.LiveBroker
	closeFails bool
	closed     bool
}

func (f *failingResolveBroker) ResolveExecution(context.Context, string) (port.ResolvedExecution, error) {
	return port.ResolvedExecution{}, errors.New("resolve unavailable")
}

func (f *failingResolveBroker) ClosePosition(ctx context.Context, req port.CloseRequest) (*port.CloseResult, error) {
	if f.closeFails {
		return nil, errors.New("close rejected")
	}
	f.closed = true
	return f.LiveBroker.ClosePosition(ctx, req)
}

func entrySignal(now time.Time) strategy.Signal {
	return strategy.Signal{
		Decision: strategy.DecisionEnter, SignalID: "sig-x", Symbol: "7203", Side: order.SideBuy,
		Quantity: 100, TakeProfitJPY: 10, StopLossJPY: 10, HoldingMode: order.HoldingIntraday,
		ConfigID: "cfg", CreatedAt: now,
	}
}

// sagaFixture は ExecuteOrder サーガの共通土台。9 本のテストが同じ 7 行を組み立て
// 直していたのを 1 か所へ寄せた。失敗注入は必ず paper broker のデコレータとして
// 書く(wrap)— 実際の約定/建玉は紙 broker が持ち、注入側は 1 メソッドだけ壊す。
type sagaFixture struct {
	now     time.Time
	paper   *broker.Paper
	trades  *repository.InMemoryTradeRepo
	posRepo *repository.InMemoryPositionRepo
	pending *safety.PendingPositions
	es      *safety.EmergencyStop
	exec    *ExecuteOrder
}

func newSaga(t *testing.T, wrap func(port.LiveBroker) port.LiveBroker) *sagaFixture {
	t.Helper()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("7203", 1000)
	f := &sagaFixture{
		now: now, paper: pb,
		posRepo: repository.NewInMemoryPositionRepo(),
		pending: safety.NewPendingPositions(),
		es:      safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil),
	}
	f.trades = repository.NewInMemoryTradeRepo()
	var brk port.LiveBroker = pb
	if wrap != nil {
		brk = wrap(pb)
	}
	f.exec = NewExecuteOrder(brk, f.posRepo, f.pending, f.es, c).
		WithCloser(repository.NewCloser(f.posRepo, f.trades))
	return f
}

// enter はこのパッケージの標準エントリー(7203 BUY)。qty だけがテストごとに違う。
func (f *sagaFixture) enter(ctx context.Context, qty int) (int64, error) {
	sig := entrySignal(f.now)
	sig.Quantity = qty
	return f.exec.Execute(ctx, ExecuteOrderInput{
		Signal: sig, Quantity: qty, ExecKind: order.ExecMarginOneday, Source: position.SourceBot,
	})
}

func newExecOrder(t *testing.T, failClose bool) (*ExecuteOrder, *safety.EmergencyStop) {
	t.Helper()
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		return &failingOCOBroker{LiveBroker: pb, failClose: failClose}
	})
	return f.exec, f.es
}

// A naked unprotected position must never fail open silently.
func TestExecuteOrder_CompensatingCloseFailureTripsEmergency(t *testing.T) {
	exec, es := newExecOrder(t, true)

	_, err := exec.Execute(context.Background(), ExecuteOrderInput{
		Signal: entrySignal(time.Now()), Quantity: 100, ExecKind: order.ExecMarginOneday, Source: position.SourceBot,
	})

	if err == nil {
		t.Fatal("expected an error from the entry saga")
	}
	if !es.Active() {
		t.Fatal("expected emergency_stop to trip after the compensating close failed (naked position)")
	}
}

// A successful compensating close is a CLEAN rollback: error, but no trip.
func TestExecuteOrder_ResolveFailureCompensates(t *testing.T) {
	var brk *failingResolveBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		brk = &failingResolveBroker{LiveBroker: pb}
		return brk
	})

	if _, err := f.enter(context.Background(), 100); err == nil {
		t.Fatal("expected an error when the fill cannot be resolved")
	}
	if !brk.closed {
		t.Fatal("saga must compensate (close) a placed-but-unresolved entry, not orphan it")
	}
	if f.es.Active() {
		t.Fatal("emergency_stop must NOT trip when the compensating close succeeded")
	}
}

func TestExecuteOrder_ResolveFailureWithCloseFailureTrips(t *testing.T) {
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		return &failingResolveBroker{LiveBroker: pb, closeFails: true}
	})

	if _, err := f.enter(context.Background(), 100); err == nil {
		t.Fatal("expected an error when both resolve and the compensating close fail")
	}
	if !f.es.Active() {
		t.Fatal("emergency_stop must trip when a possibly-naked position cannot be closed")
	}
}

// notFilledBroker reports a CONFIRMED zero-fill (ストップ高/安, halt, illiquid):
// there is no position, so nothing may be compensated or tripped.
type notFilledBroker struct {
	port.LiveBroker
	cancelled   bool
	closed      bool
	cancelFails bool // true なら cancel が業務拒否される(Cancelled:false)
	resolves    int
}

func (b *notFilledBroker) ResolveExecution(context.Context, string) (port.ResolvedExecution, error) {
	b.resolves++
	return port.ResolvedExecution{}, fmt.Errorf("entry: %w", port.ErrOrderNotFilled)
}
func (b *notFilledBroker) CancelOrder(ctx context.Context, id string) (*port.CancelResult, error) {
	b.cancelled = true
	return &port.CancelResult{OrderID: id, Cancelled: !b.cancelFails}, nil
}
func (b *notFilledBroker) ClosePosition(ctx context.Context, req port.CloseRequest) (*port.CloseResult, error) {
	b.closed = true
	return b.LiveBroker.ClosePosition(ctx, req)
}

func TestExecuteOrder_ConfirmedNoFillCancelsWithoutTripping(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("7203", 1000)
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	brk := &notFilledBroker{LiveBroker: pb}
	exec := NewExecuteOrder(brk, repository.NewInMemoryPositionRepo(), safety.NewPendingPositions(), es, c)

	_, err := exec.Execute(context.Background(), ExecuteOrderInput{
		Signal: entrySignal(now), Quantity: 100, ExecKind: order.ExecMarginOneday, Source: position.SourceBot,
	})
	if err == nil {
		t.Fatal("expected an error when the entry does not fill")
	}
	if !brk.cancelled {
		t.Fatal("a confirmed no-fill must cancel the working order")
	}
	if brk.closed {
		t.Fatal("a confirmed no-fill must NOT compensate (there is no position to close)")
	}
	if es.Active() {
		t.Fatal("a confirmed no-fill must NOT trip emergency_stop (nothing filled)")
	}
}

// 🚨 確認済ゼロ約定でも **cancel の結果は権威**(partial fill 枝と同じ規律)。
// 取消が拒否された注文は板に生きていて、数分後に約定すると台帳にも守りにも無い
// 建玉になる。再照会でも約定が見えなければ trip して人間に渡し、その銘柄は当日
// 建てない。
func TestExecuteOrder_ConfirmedNoFillCancelUnconfirmedTrips(t *testing.T) {
	ctx := context.Background()
	var brk *notFilledBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		brk = &notFilledBroker{LiveBroker: pb, cancelFails: true}
		return brk
	})

	if _, err := f.enter(ctx, 100); err == nil {
		t.Fatal("an unconfirmed cancel of an unfilled entry must abort with an error")
	}
	if brk.resolves != 2 {
		t.Fatalf("cancel 拒否のあと再照会で約定を確認する経路が無い: resolve %d 回, want 2", brk.resolves)
	}
	if brk.closed {
		t.Fatal("nothing filled: there is no position to compensate")
	}
	if !f.es.Active() {
		t.Fatal("a working entry order that cannot be cancelled must trip emergency (fail-close)")
	}
	if !f.exec.EntryBlocked("7203", f.now) {
		t.Fatal("the symbol must be blocked for the day while the uncancelled order may still fill")
	}
	if open, _ := f.posRepo.ListOpenOrClosing(ctx, "7203"); len(open) != 0 {
		t.Fatalf("no position may be frozen, got %+v", open)
	}
}

type countingOps struct{ compensations, adoptions, tpDropped int }

func (o *countingOps) IncrCompensations()       { o.compensations++ }
func (o *countingOps) IncrExternalAdoptions()   { o.adoptions++ }
func (o *countingOps) IncrProtectiveTPDropped() { o.tpDropped++ }

func TestExecuteOrder_CompensationIncrementsCounter(t *testing.T) {
	exec, _ := newExecOrder(t, false) // close succeeds → clean rollback
	ops := &countingOps{}
	exec.Ops = ops

	_, _ = exec.Execute(context.Background(), ExecuteOrderInput{
		Signal: entrySignal(time.Now()), Quantity: 100, ExecKind: order.ExecMarginOneday, Source: position.SourceBot,
	})

	if ops.compensations != 1 {
		t.Fatalf("Compensations = %d, want 1", ops.compensations)
	}
}

func TestExecuteOrder_CompensatingCloseSuccessDoesNotTrip(t *testing.T) {
	exec, es := newExecOrder(t, false)

	_, err := exec.Execute(context.Background(), ExecuteOrderInput{
		Signal: entrySignal(time.Now()), Quantity: 100, ExecKind: order.ExecMarginOneday, Source: position.SourceBot,
	})

	if err == nil {
		t.Fatal("expected an error from the entry saga")
	}
	if es.Active() {
		t.Fatal("emergency_stop must NOT trip when the position was safely rolled back")
	}
}

// partialFillBroker は resolve が返す約定数量を差し替える失敗注入。3 つに分かれて
// いた fake(部分約定 / cancel 拒否 / cancel がレースに負けて全約定)を 1 本にした
// — ResolveExecution の本体は 3 つとも同形で、CancelOrder の 2 つはバイト一致だった。
// MOCK rationale (TESTING.md 3用途): §2 failure injection at the broker boundary.
//
//	fills    … resolve の n 回目が返す約定数量。尽きたら最後の値を返し続ける。
//	           🛑 残注文があるとき execute_order は resolve を**ちょうど 2 回**呼ぶ。
//	           2 要素の fixture がその経路を拘束している(calls で実際に検査する)。
//	cancelOK … false なら残注文の cancel を拒否する(Cancelled:false を返す)。
type partialFillBroker struct {
	port.LiveBroker
	fills    []int
	cancelOK bool
	calls    int
}

func (b *partialFillBroker) ResolveExecution(ctx context.Context, orderID string) (port.ResolvedExecution, error) {
	rex, err := b.LiveBroker.ResolveExecution(ctx, orderID)
	if err != nil {
		return rex, err
	}
	b.calls++
	i := b.calls - 1
	if i >= len(b.fills) {
		i = len(b.fills) - 1
	}
	rex.FilledQuantity = b.fills[i]
	return rex, nil
}

func (b *partialFillBroker) CancelOrder(ctx context.Context, id string) (*port.CancelResult, error) {
	if b.cancelOK {
		return b.LiveBroker.CancelOrder(ctx, id)
	}
	return &port.CancelResult{OrderID: id, Cancelled: false}, nil
}

// 約定数量が正: protecting the REQUESTED quantity makes the exit over-sell.
func TestExecuteOrder_PartialFillFreezesFilledQuantity(t *testing.T) {
	ctx := context.Background()
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		return &partialFillBroker{LiveBroker: pb, fills: []int{100}, cancelOK: true}
	})

	posID, err := f.enter(ctx, 300)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	open, _ := f.posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 || open[0].ID != posID || open[0].Quantity != 100 {
		t.Fatalf("frozen quantity must be the FILLED 100, got %+v", open)
	}
	orders, _ := f.paper.GetActiveOrders(ctx, "7203")
	if len(orders) != 1 || orders[0].Quantity != 100 || orders[0].Side != order.SideSell {
		t.Fatalf("protective order must be close-side qty=100, got %+v", orders)
	}
}

// An uncancelled, unconfirmable residual must never be left to fill naked.
func TestExecuteOrder_PartialFillUncancellableResidualTrips(t *testing.T) {
	ctx := context.Background()
	// cancel 拒否 + 再 resolve でも約定数量が増えない(残注文が説明できない)。
	var brk *partialFillBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		brk = &partialFillBroker{LiveBroker: pb, fills: []int{100, 100}}
		return brk
	})

	if _, err := f.enter(ctx, 300); err == nil {
		t.Fatal("an uncancellable partial-fill residual must abort the saga with an error")
	}
	if brk.calls != 2 {
		t.Fatalf("cancel 拒否のあと再 resolve で残注文を確認する経路が消えている: resolve %d 回, want 2", brk.calls)
	}
	if !f.es.Active() {
		t.Fatal("an uncancellable residual must trip emergency (fail-close)")
	}
	if open, _ := f.posRepo.ListOpenOrClosing(ctx, "7203"); len(open) != 0 {
		t.Fatalf("no position should be frozen when the residual is unaccountable, got %+v", open)
	}
}

// 🚨 再照会で約定数量が**増えたがまだ部分約定**のとき、cancel の失敗を素通ししてはいけない。
// 従来は「増えた」枝に入ると `!cancelled` を評価せず、残注文が板に生きたまま 200 株を
// 凍結していた。
func TestExecuteOrder_PartialFillGrowsButResidualUncancelledTrips(t *testing.T) {
	ctx := context.Background()
	var brk *partialFillBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		brk = &partialFillBroker{LiveBroker: pb, fills: []int{100, 200}} // cancelOK=false
		return brk
	})

	if _, err := f.enter(ctx, 300); err == nil {
		t.Fatal("a residual that is neither cancelled nor fully filled must abort the saga")
	}
	if brk.calls != 2 {
		t.Fatalf("resolve %d 回, want 2", brk.calls)
	}
	if !f.es.Active() {
		t.Fatal("an uncancellable residual must trip emergency even when the re-resolve shows more shares")
	}
	if open, _ := f.posRepo.ListOpenOrClosing(ctx, "7203"); len(open) != 0 {
		t.Fatalf("no position may be frozen while 100 shares are still working, got %+v", open)
	}
}

// The re-resolve recovers the true FULL quantity instead of compensating.
func TestExecuteOrder_PartialFillResidualFilledIsRecoveredByReResolve(t *testing.T) {
	ctx := context.Background()
	// cancel はレースに負け、残注文は再 resolve までに約定している。
	var brk *partialFillBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		brk = &partialFillBroker{LiveBroker: pb, fills: []int{200, 300}}
		return brk
	})

	posID, err := f.enter(ctx, 300)
	if err != nil {
		t.Fatalf("a residual confirmed filled on re-resolve must succeed, got: %v", err)
	}
	if brk.calls != 2 {
		t.Fatalf("再 resolve で全約定を拾う経路が消えている: resolve %d 回, want 2", brk.calls)
	}
	if f.es.Active() {
		t.Fatal("a recovered full fill must NOT trip emergency")
	}
	open, _ := f.posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 || open[0].ID != posID || open[0].Quantity != 300 {
		t.Fatalf("frozen quantity must be the confirmed full 300, got %+v", open)
	}
}

// vanishingSubmitBroker: the submit reached the exchange and FILLED, but its HTTP
// response was lost.
type vanishingSubmitBroker struct {
	port.LiveBroker
}

func (b *vanishingSubmitBroker) PlaceOrder(ctx context.Context, req order.PlaceOrderRequest) (*port.PlaceOrderResult, error) {
	_, _ = b.LiveBroker.PlaceOrder(ctx, req) // reached the exchange and filled...
	return nil, errors.New("transport timeout")
}

// A PlaceOrder timeout after the exchange accepted must not leave a naked
// untracked position.
func TestExecuteOrder_UnconfirmedSubmitCompensatesOrphanFill(t *testing.T) {
	ctx := context.Background()
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		return &vanishingSubmitBroker{LiveBroker: pb}
	})

	if _, err := f.enter(ctx, 100); err == nil {
		t.Fatal("submit error must propagate")
	}
	if bps, _ := f.paper.GetPositions(ctx); len(bps) != 0 {
		t.Fatalf("orphan fill must be compensated (closed), broker still holds %+v", bps)
	}
	if f.es.Active() {
		t.Fatalf("clean compensation must not trip, reason=%q", f.es.Reason())
	}
	if open, _ := f.posRepo.ListOpenOrClosing(ctx, "7203"); len(open) != 0 {
		t.Fatalf("no DB position should exist, got %+v", open)
	}
}

// unverifiableSubmitBroker fails the submit AND the position listing.
type unverifiableSubmitBroker struct {
	port.LiveBroker
}

func (b *unverifiableSubmitBroker) PlaceOrder(context.Context, order.PlaceOrderRequest) (*port.PlaceOrderResult, error) {
	return nil, errors.New("transport timeout")
}

func (b *unverifiableSubmitBroker) GetPositions(context.Context) ([]port.BrokerPosition, error) {
	return nil, errors.New("api down")
}

// Never assume a lost submit did not fill: trip when the scan itself fails.
func TestExecuteOrder_UnverifiableSubmitTrips(t *testing.T) {
	ctx := context.Background()
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		return &unverifiableSubmitBroker{LiveBroker: pb}
	})

	if _, err := f.enter(ctx, 100); err == nil {
		t.Fatal("submit error must propagate")
	}
	if !f.es.Active() {
		t.Fatal("unverifiable submit must trip emergency (fail-close)")
	}
}

type observingBroker struct {
	port.LiveBroker
	onResolve func()
}

func (b *observingBroker) ResolveExecution(ctx context.Context, orderID string) (port.ResolvedExecution, error) {
	if b.onResolve != nil {
		b.onResolve()
	}
	return b.LiveBroker.ResolveExecution(ctx, orderID)
}

// The symbol stays pending for the whole saga so Reconcile cannot mis-adopt the
// in-flight fill during the resolve window.
func TestExecuteOrder_SymbolPendingDuringSaga(t *testing.T) {
	ctx := context.Background()
	observed := false
	var f *sagaFixture
	f = newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		return &observingBroker{LiveBroker: pb, onResolve: func() {
			observed = f.pending.IsPendingSymbol("7203")
		}}
	})

	if _, err := f.enter(ctx, 100); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !observed {
		t.Fatal("symbol must be pending while the fill resolves")
	}
	if f.pending.IsPendingSymbol("7203") {
		t.Fatal("symbol pending must clear when the saga ends")
	}
}

type ocoSpyBroker struct {
	port.LiveBroker
	got port.OCOCloseOrderInput
}

func (b *ocoSpyBroker) PlaceSettleOCO(ctx context.Context, in port.OCOCloseOrderInput) (string, error) {
	b.got = in
	return b.LiveBroker.PlaceSettleOCO(ctx, in)
}

// 守りの注文期日は entry saga が broker まで運ぶ。ここで落ちると、多日保有の
// 建玉は**建てた日の引けで守りが消える**。決めるのは呼び出し側
// (休場カレンダーを持つ層)で、executor は運ぶだけ。
func TestExecuteOrder_CarriesSettleExpiryToBroker(t *testing.T) {
	ctx := context.Background()
	var spy *ocoSpyBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		spy = &ocoSpyBroker{LiveBroker: pb}
		return spy
	})
	want := time.Date(2026, 6, 30, 0, 0, 0, 0, clock.JST)

	sig := entrySignal(f.now)
	sig.Quantity = 100
	if _, err := f.exec.Execute(ctx, ExecuteOrderInput{
		Signal: sig, Quantity: 100, ExecKind: order.ExecMarginGeneral,
		Source: position.SourceBot, OCOExpireOn: want,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !spy.got.ExpireOn.Equal(want) {
		t.Fatalf("PlaceSettleOCO の ExpireOn = %v, want %v — 守りが当日期限のまま出ている", spy.got.ExpireOn, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// live 事故(同一銘柄で買い→売りを5往復)の回帰。
//
// 起点は「TP が値幅制限の外 → 守りの発注が拒否 → 約定済みの建玉を補償で閉じる」。
// **補償で閉じた事実は台帳に何も書かない**ので、次のティックでゲートは全部通り
// (ナンピン禁止も建玉数も日次損失も連敗も、全部台帳を読む)、また建てた。
// ここで縛るのは「同じ銘柄で同じ日に entry saga を巻き戻したら、その日はもう建てない」。
// ─────────────────────────────────────────────────────────────────────────────

func TestExecuteOrder_RollbackBlocksTheSymbolForTheRestOfTheDay(t *testing.T) {
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		return &failingOCOBroker{LiveBroker: pb} // OCO 失敗・補償の close は成功(= 今日と同じ形)
	})
	exec, now := f.exec, f.now

	if exec.EntryBlocked("7203", now) {
		t.Fatal("何も起きていないのに block されている")
	}
	_, err := exec.Execute(context.Background(), ExecuteOrderInput{
		Signal: entrySignal(now), Quantity: 100, ExecKind: order.ExecMarginOneday, Source: position.SourceBot,
	})
	if err == nil {
		t.Fatal("entry saga はエラーで終わるはず")
	}
	if !exec.EntryBlocked("7203", now) {
		t.Fatal("巻き戻した銘柄が block されていない — これが 5 往復の穴")
	}
	// 他の銘柄は巻き込まない(全体を止めるのは emergency_stop の仕事)。
	if exec.EntryBlocked("6758", now) {
		t.Fatal("無関係の銘柄まで block した")
	}
	// 翌営業日には解ける(値幅制限は基準値段が変われば変わる = 日単位の事象)。
	if exec.EntryBlocked("7203", now.AddDate(0, 0, 1)) {
		t.Fatal("翌日まで block が残っている")
	}
}

// 🛑 巻き戻しが起きても emergency は落とさない(既存の設計)。銘柄単位の block で
// 足りる — 1 銘柄の値幅制限のために口座全体の live を止めるのは過剰。
func TestExecuteOrder_RollbackDoesNotTripEmergency(t *testing.T) {
	exec, es := newExecOrder(t, false)

	_, _ = exec.Execute(context.Background(), ExecuteOrderInput{
		Signal: entrySignal(time.Now()), Quantity: 100, ExecKind: order.ExecMarginOneday, Source: position.SourceBot,
	})

	if es.Active() {
		t.Fatal("clean rollback で emergency を落としてはいけない(銘柄 block で足りる)")
	}
}

// TP が本日の値幅制限の外なら、**TP 脚を落として SL だけ broker 側に置く**。
// entry ごと諦めない(CLAUDE.md「SL 欠落は致命的、TP 欠落は機会損失のみ」)。
// これをしないと、急落が深いほど TP(25日線)が遠い BNF は構造的に建たなくなる。
func TestExecuteOrder_DropsTakeProfitLegOutsidePriceLimit(t *testing.T) {
	ctx := context.Background()
	var spy *ocoSpyBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		spy = &ocoSpyBroker{LiveBroker: pb}
		return spy
	})

	f.paper.SetPrice("7203", 5381) // 約定値 = 5,381(TP/SL は約定値から作られる)
	sig := entrySignal(f.now)
	sig.Quantity = 100
	sig.EntryPrice = 5381
	sig.TakeProfitJPY = 1054 // → TP 6,435
	sig.StopLossJPY = 583    // → SL 4,798
	// 基準値段 5,352 → 帯 4,352〜6,352。TP は外、SL は内側。
	if _, err := f.exec.Execute(ctx, ExecuteOrderInput{
		Signal: sig, Quantity: 100, ExecKind: order.ExecMarginGeneral,
		Source: position.SourceBot, PriceLimitRef: 5352,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if spy.got.TakeProfit != 0 {
		t.Fatalf("TP 脚 = %v, want 0 — 値幅制限の外の指値を board に出そうとしている(立花は拒否する)", spy.got.TakeProfit)
	}
	if spy.got.StopLoss <= 0 {
		t.Fatalf("SL 脚が落ちている(%v)— 守りは必ず broker 側に置く", spy.got.StopLoss)
	}
}

// 🚨 TP 脚を board から落としても、**台帳の take_profit_price は残す**。
// ここが 0 で凍結されると broker 側にも bot 側にも TP が無い建玉になり、
// price_limit_gate.go / CLAUDE.md / FAILURE_MODES.md が約束する
// 「TP は OnTick が引き継ぐ」が成立しない。
func TestExecuteOrder_DroppedTakeProfitStaysInTheLedger(t *testing.T) {
	ctx := context.Background()
	var spy *ocoSpyBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		spy = &ocoSpyBroker{LiveBroker: pb}
		return spy
	})

	f.paper.SetPrice("7203", 5381)
	sig := entrySignal(f.now)
	sig.Quantity = 100
	sig.EntryPrice = 5381
	sig.TakeProfitJPY = 1054 // → TP 6,435(帯 4,352〜6,352 の外)
	sig.StopLossJPY = 583
	id, err := f.exec.Execute(ctx, ExecuteOrderInput{
		Signal: sig, Quantity: 100, ExecKind: order.ExecMarginGeneral,
		Source: position.SourceBot, PriceLimitRef: 5352,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if spy.got.TakeProfit != 0 {
		t.Fatalf("TP 脚 = %v, want 0(board には出さない)", spy.got.TakeProfit)
	}
	p, err := f.posRepo.GetByID(ctx, id)
	if err != nil || p == nil {
		t.Fatalf("GetByID: %v (p=%v)", err, p)
	}
	if p.TakeProfitPrice <= 0 {
		t.Fatalf("台帳の take_profit_price = %v — 0 で凍結すると OnTick が引き継げず、"+
			"broker 側にも bot 側にも TP が無い建玉になる", p.TakeProfitPrice)
	}
	if p.StopLossPrice <= 0 {
		t.Fatalf("台帳の stop_loss_price = %v", p.StopLossPrice)
	}
}

// 帯の内側の TP はそのまま出す(落とすのは置けないときだけ)。
func TestExecuteOrder_KeepsTakeProfitLegInsidePriceLimit(t *testing.T) {
	ctx := context.Background()
	var spy *ocoSpyBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		spy = &ocoSpyBroker{LiveBroker: pb}
		return spy
	})

	f.paper.SetPrice("7203", 5381)
	sig := entrySignal(f.now)
	sig.Quantity = 100
	sig.EntryPrice = 5381
	sig.TakeProfitJPY = 500 // → TP 5,881 は帯の内側
	sig.StopLossJPY = 583
	if _, err := f.exec.Execute(ctx, ExecuteOrderInput{
		Signal: sig, Quantity: 100, ExecKind: order.ExecMarginGeneral,
		Source: position.SourceBot, PriceLimitRef: 5352,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if spy.got.TakeProfit <= 0 {
		t.Fatalf("帯の内側の TP を落とした(%v)", spy.got.TakeProfit)
	}
}

// 基準値段が渡らない構成(backtest 等)では従来どおり TP を出す — 値幅制限を
// 判定できないことを理由に守りの形を勝手に変えない。
func TestExecuteOrder_WithoutPriceLimitRefKeepsTakeProfit(t *testing.T) {
	ctx := context.Background()
	var spy *ocoSpyBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		spy = &ocoSpyBroker{LiveBroker: pb}
		return spy
	})

	f.paper.SetPrice("7203", 5381)
	sig := entrySignal(f.now)
	sig.Quantity = 100
	sig.EntryPrice = 5381
	sig.TakeProfitJPY = 1054
	sig.StopLossJPY = 583
	if _, err := f.exec.Execute(ctx, ExecuteOrderInput{
		Signal: sig, Quantity: 100, ExecKind: order.ExecMarginGeneral,
		Source: position.SourceBot, // PriceLimitRef 未設定
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if spy.got.TakeProfit <= 0 {
		t.Fatalf("基準値段が無いのに TP を落とした(%v)", spy.got.TakeProfit)
	}
}

// 🛑 **今回の恒久対策**。約定した後に守りを置けず巻き戻した往復を台帳に残す。
// これが無いと、再入場を止めうるゲート(cooldown / max_trades_in_this_window /
// daily_loss / account_daily_loss / consecutive_losses)は**全部が台帳を読む**ので
// 同時に無効化される — live で 5 往復した直接原因。
func TestExecuteOrder_RollbackIsBookedInTheLedger(t *testing.T) {
	ctx := context.Background()
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		return &failingOCOBroker{LiveBroker: pb} // OCO 失敗 → 補償 close は成功
	})
	f.paper.SetPrice("7203", 1000)

	if _, err := f.enter(ctx, 100); err == nil {
		t.Fatal("entry saga はエラーで終わるはず")
	}

	trades, err := f.trades.ListClosedSince(ctx, f.now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("list trades: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("台帳の trade = %d 件, want 1 — 実弾の往復が記録されていない", len(trades))
	}
	if trades[0].CloseReason != "entry_compensated" {
		t.Fatalf("close_reason = %q, want entry_compensated(戦略の出口と区別できないとエッジ台帳が汚れる)", trades[0].CloseReason)
	}
	if trades[0].Symbol != "7203" || trades[0].Quantity != 100 {
		t.Fatalf("trade の中身が違う: %+v", trades[0])
	}
	// 建玉は閉じた状態で残る(open のまま残すとナンピン禁止が永久に閉じる)。
	open, err := f.posRepo.ListOpenOrClosing(ctx, "7203")
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("OPEN/CLOSING の建玉が %d 件残っている: %+v", len(open), open)
	}
}

// 決済の約定を確認できないときは trade を書かない = CLOSING のまま reconcile へ渡す。
// 「受理」を「約定」と読んで台帳に幽霊決済を書かない(立花は決済約定が非同期)。
func TestExecuteOrder_RollbackWithUnconfirmedSettleLeavesClosing(t *testing.T) {
	ctx := context.Background()
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		return &unconfirmedSettleOCOBroker{LiveBroker: pb}
	})

	if _, err := f.enter(ctx, 100); err == nil {
		t.Fatal("entry saga はエラーで終わるはず")
	}

	trades, _ := f.trades.ListClosedSince(ctx, f.now.Add(-time.Hour))
	if len(trades) != 0 {
		t.Fatalf("約定未確認なのに trade を書いた: %+v", trades)
	}
	open, _ := f.posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 || open[0].Status != position.StatusClosing {
		t.Fatalf("CLOSING で残っていない(reconcile が拾えない): %+v", open)
	}
	// 🚨 **CLOSING で残るだけでは足りない。** 守りは既に cancel 済みなので、決済が
	// 板にも載っていなければこの建玉は裸。paper の板には決済側の注文が残らないため
	// ここは裸の判定に落ち、警報が鳴らねばならない(closeOne で塞いだ
	// 穴が、補償経路にはそのまま残っていた)。
	if !f.es.Active() {
		t.Fatal("補償の決済が未確認かつ板にも無い = 建玉が裸。警報を鳴らさねばならない")
	}
}

// 🚨 立花は信用建玉を **銘柄単位に集約**する(broker_position_id = "shinyo:4704" 1 本)。
// 送信が transport エラーになったとき、台帳に無い同 side 建玉を「自分の孤児」と決め打ちして
// bp.Quantity ぶん丸ごと閉じると、**人間が手で建てた玉まで売る**。
// 実際にその状態があった(台帳が空のまま人間が 4704 を手動で信用買い)。
// 我々が作りえた孤児は多くても発注数量ぶんなので、それを超える建玉は「我々のものだけでは
// ない」= 推測しない → emergency trip して人間に渡す。
func TestExecuteOrder_UnconfirmedSubmitDoesNotCloseAForeignPosition(t *testing.T) {
	ctx := context.Background()
	var brk *submitFailsWithBiggerBookBroker
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		brk = &submitFailsWithBiggerBookBroker{LiveBroker: pb}
		return brk
	})

	sig := entrySignal(f.now)
	sig.Quantity = 100
	_, err := f.exec.Execute(ctx, ExecuteOrderInput{
		Signal: sig, Quantity: 100, ExecKind: order.ExecMarginOneday, Source: position.SourceBot,
	})
	if err == nil {
		t.Fatal("transport エラーなのでサーガはエラーで終わるはず")
	}
	if !f.es.Active() {
		t.Fatal("発注数量より大きい建玉を前に compensate せず trip すること(他人の玉を売らない)")
	}
	// 🛑 本題はここ。**決済を一度も出していない**こと(trip したかどうかではない)。
	if brk.closeCalls != 0 {
		t.Fatalf("ClosePosition を %d 回出した — 人間の建玉を含む集約建玉を売りに行っている", brk.closeCalls)
	}
}

// submitFailsWithBiggerBookBroker: 送信は transport エラー、broker には**発注数量より
// 大きい**同 side 建玉がある(人間の手動建玉 + 我々の孤児が集約された形)。
// MOCK rationale (TESTING.md 3用途): §2 failure injection at the broker boundary.
type submitFailsWithBiggerBookBroker struct {
	port.LiveBroker
	closeCalls int
}

func (b *submitFailsWithBiggerBookBroker) ClosePosition(ctx context.Context, req port.CloseRequest) (*port.CloseResult, error) {
	b.closeCalls++
	return b.LiveBroker.ClosePosition(ctx, req)
}

func (b *submitFailsWithBiggerBookBroker) PlaceOrder(context.Context, order.PlaceOrderRequest) (*port.PlaceOrderResult, error) {
	return nil, errors.New("transport failure")
}

func (b *submitFailsWithBiggerBookBroker) GetActiveOrders(context.Context, string) ([]order.Order, error) {
	return nil, nil
}

func (b *submitFailsWithBiggerBookBroker) GetPositions(context.Context) ([]port.BrokerPosition, error) {
	return []port.BrokerPosition{{
		BrokerPositionID: "shinyo:7203", Symbol: "7203", Side: order.SideBuy,
		Quantity: 200, EntryPrice: 1000, ExecKind: order.ExecMarginOneday, // 100 は人間の玉
	}}, nil
}

// 🚨 補償で巻き戻した往復の建玉も **strategy_name を凍結する**。
// 決済が確認できず CLOSING で座礁すると `strategy_name = ”` の建玉が残り、
// ナンピン禁止も arm 判定も「戦略不明 = 全戦略を建玉中とみなす」に倒れるので、
// **その銘柄の全アームが無期限にブロックされる**。
func TestExecuteOrder_CompensatedRoundTripFreezesTheStrategyName(t *testing.T) {
	ctx := context.Background()
	f := newSaga(t, func(pb port.LiveBroker) port.LiveBroker {
		return &failingOCOBroker{LiveBroker: pb} // OCO 失敗 → 補償 close
	})
	sig := entrySignal(f.now)
	sig.Quantity = 100
	sig.StrategyName = "bnf_reversion" // entrySignal() は戦略名を持たないので、凍結される値をここで与える
	if _, err := f.exec.Execute(ctx, ExecuteOrderInput{
		Signal: sig, Quantity: 100, ExecKind: order.ExecMarginGeneral, Source: position.SourceBot,
	}); err == nil {
		t.Fatal("OCO 失敗が成功を返した")
	}
	// 巻き戻し済みなので OPEN には残らない。台帳の行は closed 側にある —
	// そこを読まないとこのテストは何も縛らない。
	trades, err := f.trades.ListClosedSince(ctx, f.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 1 || trades[0].CloseReason != port.CloseReasonEntryCompensated {
		t.Fatalf("巻き戻した往復が entry_compensated として台帳に 1 行残るべき: %+v", trades)
	}
	p, err := f.posRepo.GetByID(ctx, trades[0].PositionID)
	if err != nil {
		t.Fatal(err)
	}
	if p.StrategyName == "" {
		t.Fatalf("巻き戻した往復の建玉に strategy_name が無い(%+v)— その銘柄の全アームが無期限にブロックされる", p)
	}
	if p.StrategyName != "bnf_reversion" {
		t.Fatalf("strategy_name = %q, want %q(凍結値)", p.StrategyName, "bnf_reversion")
	}
}
