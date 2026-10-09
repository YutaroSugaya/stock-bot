package command

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
)

// reservationBroker models real Japanese-broker 拘束: a working protective order
// reserves the 建玉 quantity, so ClosePosition is REJECTED while it rests. The
// paper broker does NOT model this, which silently masked the bricked-exit defect.
//
// foreign models a close-side order the LEDGER DOES NOT KNOW: a 決済注文 a human
// placed in the broker's app, or a leg id that went stale. It 拘束s the 建玉
// exactly like the bot's own OCO, and cancelling the RECORDED legs does not
// release it.
type reservationBroker struct {
	*broker.Paper
	mu              sync.Mutex
	reserved        map[string]bool        // brokerPositionID -> protective order resting
	legOwner        map[string]string      // protective order id (root/tp/sl) -> brokerPositionID
	foreign         map[string]order.Order // order id -> resting close-side order no positions_live row points at
	foreignOwner    map[string]string      // order id -> brokerPositionID it 拘束s
	closeRejections int
}

func newReservationBroker(p *broker.Paper) *reservationBroker {
	return &reservationBroker{Paper: p, reserved: map[string]bool{}, legOwner: map[string]string{},
		foreign: map[string]order.Order{}, foreignOwner: map[string]string{}}
}

// injectForeignSettleOrder puts a close-side order on the board that the ledger
// never recorded.
func (b *reservationBroker) injectForeignSettleOrder(o order.Order, bpID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.foreign[o.OrderID] = o
	b.foreignOwner[o.OrderID] = bpID
}

// GetActiveOrders is what the board really shows: the bot's own OCO AND anything
// else resting on the same symbol.
func (b *reservationBroker) GetActiveOrders(ctx context.Context, symbol string) ([]order.Order, error) {
	out, err := b.Paper.GetActiveOrders(ctx, symbol)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, o := range b.foreign {
		if o.Symbol == symbol {
			out = append(out, o)
		}
	}
	return out, nil
}

func (b *reservationBroker) PlaceSettleOCO(ctx context.Context, in port.OCOCloseOrderInput) (string, error) {
	root, err := b.Paper.PlaceSettleOCO(ctx, in)
	if err == nil {
		b.mu.Lock()
		b.reserved[in.BrokerPositionID] = true
		b.legOwner[root] = in.BrokerPositionID
		b.mu.Unlock()
	}
	return root, err
}

func (b *reservationBroker) ResolveSettleLegs(ctx context.Context, bpID, sym string) (string, string, error) {
	tp, sl, err := b.Paper.ResolveSettleLegs(ctx, bpID, sym)
	if err == nil {
		b.mu.Lock()
		if tp != "" {
			b.legOwner[tp] = bpID
		}
		if sl != "" {
			b.legOwner[sl] = bpID
		}
		b.mu.Unlock()
	}
	return tp, sl, err
}

func (b *reservationBroker) CancelOrder(ctx context.Context, id string) (*port.CancelResult, error) {
	b.mu.Lock()
	if bp, ok := b.legOwner[id]; ok {
		b.reserved[bp] = false // cancelling the protective order releases the reservation
	}
	delete(b.foreign, id)
	delete(b.foreignOwner, id)
	b.mu.Unlock()
	return b.Paper.CancelOrder(ctx, id)
}

func (b *reservationBroker) ClosePosition(ctx context.Context, req port.CloseRequest) (*port.CloseResult, error) {
	b.mu.Lock()
	reserved := b.reserved[req.BrokerPositionID]
	for _, bp := range b.foreignOwner {
		if bp == req.BrokerPositionID {
			reserved = true // a stray close-side order 拘束s the 建玉 just the same
		}
	}
	if reserved {
		b.closeRejections++
	}
	b.mu.Unlock()
	if reserved {
		return &port.CloseResult{Accepted: false, Message: "quantity reserved by a working settle order (拘束)"}, nil
	}
	return b.Paper.ClosePosition(ctx, req)
}

func openOne(t *testing.T, ctx context.Context, brk port.LiveBroker, posRepo port.PositionRepository, es *safety.EmergencyStop, c clock.Clock, mode order.HoldingMode, ek order.ExecKind) int64 {
	t.Helper()
	exec := NewExecuteOrder(brk, posRepo, safety.NewPendingPositions(), es, c)
	sig := entrySignal(c())
	sig.HoldingMode = mode
	id, err := exec.Execute(ctx, ExecuteOrderInput{Signal: sig, Quantity: 100, ExecKind: ek, Source: position.SourceBot})
	if err != nil {
		t.Fatalf("open position: %v", err)
	}
	return id
}

// Closing cleanly against a 拘束-modelling broker is impossible without
// cancel-before-close.
func TestForceFlatten_CancelsProtectiveLegsBeforeClose(t *testing.T) {
	ctx := context.Background()
	openAt := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(openAt), 0, 0)
	pb.SetPrice("7203", 1000)
	brk := newReservationBroker(pb)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	closer := repository.NewCloser(posRepo, tradeRepo)
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)

	openOne(t, ctx, brk, posRepo, es, clock.Fixed(openAt), order.HoldingIntraday, order.ExecMarginOneday)

	nearClose := time.Date(2026, 6, 17, 14, 51, 0, 0, clock.JST)
	c := clock.Fixed(nearClose)
	flat := NewForceFlatten(brk, posRepo, closer, es, tokyoHours(), c)
	summary := marketSummaryAt(1000)

	n, err := flat.FlattenIfNearClose(ctx, "7203", summary)
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 flattened, got %d (the reserved close was rejected — cancel-before-close missing)", n)
	}
	if open, _ := posRepo.ListOpenOrClosing(ctx, "7203"); len(open) != 0 {
		t.Fatalf("position must be closed, still open/closing: %+v", open)
	}
	if es.Active() {
		t.Fatalf("a clean cancel-then-close must not trip, reason=%q", es.Reason())
	}
}

func TestManageOpenPositions_CancelsProtectiveLegsBeforeClose(t *testing.T) {
	ctx := context.Background()
	openAt := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(openAt), 0, 0)
	pb.SetPrice("7203", 1000)
	brk := newReservationBroker(pb)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	closer := repository.NewCloser(posRepo, tradeRepo)
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)

	exec := NewExecuteOrder(brk, posRepo, safety.NewPendingPositions(), es, clock.Fixed(openAt))
	sig := entrySignal(openAt)
	sig.MaxHoldMinutes = 5
	if _, err := exec.Execute(ctx, ExecuteOrderInput{Signal: sig, Quantity: 100, ExecKind: order.ExecMarginOneday, Source: position.SourceBot}); err != nil {
		t.Fatalf("open: %v", err)
	}

	later := openAt.Add(10 * time.Minute) // past max-hold
	mgr := NewManageOpenPositions(brk, posRepo, closer, es, clock.Fixed(later), 0)
	if err := mgr.OnTick(ctx, "7203", marketSummaryAt(1000)); err != nil {
		t.Fatalf("ontick: %v", err)
	}
	if open, _ := posRepo.ListOpenOrClosing(ctx, "7203"); len(open) != 0 {
		t.Fatalf("max-hold exit must close the position against a reservation broker, still open: %+v", open)
	}
	if es.Active() {
		t.Fatalf("clean exit must not trip, reason=%q", es.Reason())
	}
}

// 🛑 板と台帳がズレても決済は通らなければならない。実口座: ある銘柄の
// 板に載っていた決済注文と positions_live が指す注文番号が食い違っており、記録した
// leg だけを cancel しても 拘束 は解けない → 返済は拒否され、close_rejected_unprotected
// で trip したうえで建玉が CLOSING のまま取り残される。守りの cancel は台帳の記憶では
// なく**板を正**にする。
func TestManageOpenPositions_CancelsRestingSettleOrderTheLedgerNeverRecorded(t *testing.T) {
	ctx := context.Background()
	openAt := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(openAt), 0, 0)
	pb.SetPrice("7203", 1000)
	brk := newReservationBroker(pb)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	closer := repository.NewCloser(posRepo, tradeRepo)
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)

	exec := NewExecuteOrder(brk, posRepo, safety.NewPendingPositions(), es, clock.Fixed(openAt))
	sig := entrySignal(openAt)
	sig.MaxHoldMinutes = 5
	if _, err := exec.Execute(ctx, ExecuteOrderInput{Signal: sig, Quantity: 100, ExecKind: order.ExecMarginOneday, Source: position.SourceBot}); err != nil {
		t.Fatalf("open: %v", err)
	}

	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 {
		t.Fatalf("expected 1 open position, got %d", len(open))
	}
	p := open[0]
	brk.injectForeignSettleOrder(order.Order{
		OrderID: "90000001", Symbol: "7203", Side: p.Side.Opposite(),
		Quantity: p.Quantity, Price: 1456.5, Status: "WORKING",
	}, p.BrokerPositionID)

	later := openAt.Add(10 * time.Minute) // past max-hold
	mgr := NewManageOpenPositions(brk, posRepo, closer, es, clock.Fixed(later), 0)
	if err := mgr.OnTick(ctx, "7203", marketSummaryAt(1000)); err != nil {
		t.Fatalf("ontick: %v", err)
	}
	if left, _ := posRepo.ListOpenOrClosing(ctx, "7203"); len(left) != 0 {
		t.Fatalf("exit must cancel the unrecorded resting order and close; still open/closing: %+v", left)
	}
	if es.Active() {
		t.Fatalf("a stray settle order must not brick the exit, reason=%q", es.Reason())
	}
}

// failingInsertRepo fails Insert AFTER the OCO is placed, so compensate() runs
// with a RESTING protective order.
type failingInsertRepo struct {
	*repository.InMemoryPositionRepo
}

func (failingInsertRepo) Insert(context.Context, port.PositionInsertInput) (int64, error) {
	return 0, errors.New("pg: insert failed")
}

// Without cancelling the resting OCO first, the rollback close is rejected by the
// 拘束 and orphans the position.
func TestExecuteOrder_CompensateCancelsRestingOCOBeforeClose(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(now), 0, 0)
	pb.SetPrice("7203", 1000)
	brk := newReservationBroker(pb)
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	posRepo := failingInsertRepo{repository.NewInMemoryPositionRepo()}
	exec := NewExecuteOrder(brk, posRepo, safety.NewPendingPositions(), es, clock.Fixed(now))

	_, err := exec.Execute(ctx, ExecuteOrderInput{Signal: entrySignal(now), Quantity: 100, ExecKind: order.ExecMarginOneday, Source: position.SourceBot})
	if err == nil {
		t.Fatal("insert failure must surface as an error")
	}
	if es.Active() {
		t.Fatalf("compensate must cancel the OCO then close cleanly (no trip), reason=%q", es.Reason())
	}
	if bps, _ := pb.GetPositions(ctx); len(bps) != 0 {
		t.Fatalf("compensate must close the position, broker still holds %+v", bps)
	}
}

func marketSummaryAt(price float64) *market.MarketSummary {
	return &market.MarketSummary{
		Symbol: "7203", TickSize: market.TickSize(price),
		CurrentRate: market.CurrentRate{Last: price, Bid: price, Ask: price, SpreadTicks: 1},
	}
}
