package command

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
)

// liveCloseBroker models 立花's settle path: ClosePosition returns an accepted
// order with NO fill price (`FilledPrice: 0` — 同期には返らない, port 契約) and the
// caller must resolve it. The paper broker cannot expose this because it always
// answers with a price, which is why the defect was invisible in paper.
type liveCloseBroker struct {
	*broker.Paper
	mu           sync.Mutex
	closeOrders  map[string]bool // order ids handed out by ClosePosition
	cancelled    []string
	resolveCalls int
	// outcome answers ResolveExecution for CLOSE orders only.
	outcome func() (port.ResolvedExecution, error)
}

func newLiveCloseBroker(p *broker.Paper, outcome func() (port.ResolvedExecution, error)) *liveCloseBroker {
	return &liveCloseBroker{Paper: p, closeOrders: map[string]bool{}, outcome: outcome}
}

// 立花と同じ宣言(決済の約定は同期に返らない)。
func (b *liveCloseBroker) SettleFillsAsync() bool { return true }

func (b *liveCloseBroker) ClosePosition(_ context.Context, req port.CloseRequest) (*port.CloseResult, error) {
	if req.Quantity <= 0 {
		return nil, fmt.Errorf("non-positive close quantity %d", req.Quantity)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := fmt.Sprintf("close-%d", len(b.closeOrders)+1)
	b.closeOrders[id] = true
	// The 建玉 stays with the broker: whether it goes away is decided by the fill,
	// which is exactly what the caller has to resolve.
	return &port.CloseResult{OrderID: id, Accepted: true, FilledPrice: 0}, nil
}

func (b *liveCloseBroker) ResolveExecution(ctx context.Context, orderID string) (port.ResolvedExecution, error) {
	b.mu.Lock()
	isClose := b.closeOrders[orderID]
	if isClose {
		b.resolveCalls++
	}
	b.mu.Unlock()
	if isClose {
		return b.outcome()
	}
	return b.Paper.ResolveExecution(ctx, orderID)
}

func (b *liveCloseBroker) CancelOrder(ctx context.Context, id string) (*port.CancelResult, error) {
	b.mu.Lock()
	b.cancelled = append(b.cancelled, id)
	b.mu.Unlock()
	return b.Paper.CancelOrder(ctx, id)
}

func (b *liveCloseBroker) closeOrderCancelled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range b.cancelled {
		if b.closeOrders[id] {
			return true
		}
	}
	return false
}

type exitFixture struct {
	ctx      context.Context
	now      time.Time
	brk      *liveCloseBroker
	posRepo  *repository.InMemoryPositionRepo
	trades   *repository.InMemoryTradeRepo
	es       *safety.EmergencyStop
	exec     closeExecutor
	position position.Position
}

// newExitFixture opens one position against a live-shaped broker and returns the
// closeExecutor under test.
func newExitFixture(t *testing.T, outcome func() (port.ResolvedExecution, error)) *exitFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(now), 0, 0)
	pb.SetPrice("7203", 1000)
	brk := newLiveCloseBroker(pb, outcome)
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)

	openOne(t, ctx, brk, posRepo, es, clock.Fixed(now), order.HoldingMultiday, order.ExecMarginSystem)
	open, err := posRepo.ListOpenOrClosing(ctx, "7203")
	if err != nil || len(open) != 1 {
		t.Fatalf("expected 1 open position, got %+v (%v)", open, err)
	}
	return &exitFixture{
		ctx: ctx, now: now, brk: brk, posRepo: posRepo, trades: trades, es: es,
		exec:     closeExecutor{broker: brk, posRepo: posRepo, closer: repository.NewCloser(posRepo, trades), emergency: es},
		position: open[0],
	}
}

func (f *exitFixture) closedTrades(t *testing.T) []port.TradeRecord {
	t.Helper()
	list, err := f.trades.ListClosedSince(f.ctx, f.now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("list trades: %v", err)
	}
	return list
}

func (f *exitFixture) statusOf(t *testing.T) position.Status {
	t.Helper()
	list, err := f.posRepo.ListOpenOrClosing(f.ctx, "7203")
	if err != nil {
		t.Fatalf("list positions: %v", err)
	}
	if len(list) == 0 {
		return position.StatusClosed
	}
	return list[0].Status
}

// 🚨 The defect this test pins: a settle MARKET order that fills ZERO shares
// (ストップ安の張り付き)would be booked as a CLOSED trade at
// the observed price. The ledger gains a ghost close and the 建玉, still at the
// broker, is re-adopted as external = permanently unmanaged.
func TestCloseOne_ZeroFill_BooksNothingAndLeavesClosing(t *testing.T) {
	f := newExitFixture(t, func() (port.ResolvedExecution, error) {
		return port.ResolvedExecution{}, fmt.Errorf("close order: %w", port.ErrOrderNotFilled)
	})

	ok, err := f.exec.closeOne(f.ctx, f.position, 900, "stop_loss", f.now.Add(time.Hour))
	if err != nil {
		t.Fatalf("a confirmed zero fill is not an error condition: %v", err)
	}
	if ok {
		t.Fatal("closeOne must report false: nothing was closed")
	}
	if got := f.closedTrades(t); len(got) != 0 {
		t.Fatalf("no trade may be booked for an unfilled close, got %+v", got)
	}
	if st := f.statusOf(t); st != position.StatusClosing {
		t.Fatalf("position must stay CLOSING for reconcile, got %q", st)
	}
	// Deliberately asymmetric with the entry saga (which cancels and aborts):
	// ストップ安の引けには比例配分があり、板に出ている売り注文にしか配分されない。
	// Cancelling here throws away the only remaining way out.
	if f.brk.closeOrderCancelled() {
		t.Fatal("the unfilled settle order must stay resting on the book (比例配分への唯一の参加経路)")
	}
}

// A resolved fill is authoritative for the ledger: the observed 時価 is only a
// fallback, and booking it when the broker reported a different fill silently
// falsifies net.
func TestCloseOne_BooksResolvedFillPriceAndFee(t *testing.T) {
	f := newExitFixture(t, func() (port.ResolvedExecution, error) {
		return port.ResolvedExecution{OrderID: "close-1", FilledPrice: 940, FilledQuantity: 100, FeeJPY: 55}, nil
	})

	ok, err := f.exec.closeOne(f.ctx, f.position, 900, "stop_loss", f.now.Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("closeOne(ok=%v): %v", ok, err)
	}
	got := f.closedTrades(t)
	if len(got) != 1 {
		t.Fatalf("expected 1 booked trade, got %+v", got)
	}
	if got[0].ClosePrice != 940 {
		t.Fatalf("ClosePrice = %v, want the resolved fill 940 (not the observed 900)", got[0].ClosePrice)
	}
	if got[0].FeeJPY != 55 {
		t.Fatalf("FeeJPY = %v, want the resolved settle fee 55", got[0].FeeJPY)
	}
	if st := f.statusOf(t); st != position.StatusClosed {
		t.Fatalf("position must be CLOSED, got %q", st)
	}
}

// Unresolvable (transport error, deadline) is NOT "did not fill": the shares may
// be gone. Book nothing and leave it CLOSING so reconcile decides.
func TestCloseOne_UnresolvableFill_BooksNothing(t *testing.T) {
	f := newExitFixture(t, func() (port.ResolvedExecution, error) {
		return port.ResolvedExecution{}, errors.New("tachibana: order not resolvable before deadline")
	})

	ok, _ := f.exec.closeOne(f.ctx, f.position, 900, "stop_loss", f.now.Add(time.Hour))
	if ok {
		t.Fatal("closeOne must not report success when the fill cannot be confirmed")
	}
	if got := f.closedTrades(t); len(got) != 0 {
		t.Fatalf("no trade may be booked for an unconfirmable close, got %+v", got)
	}
	if st := f.statusOf(t); st != position.StatusClosing {
		t.Fatalf("position must stay CLOSING, got %q", st)
	}
}

// 約定数量が正, on the settle side too: booking the full quantity when only part
// filled reports a flat position while shares are still held.
func TestCloseOne_PartialFill_BooksNothing(t *testing.T) {
	f := newExitFixture(t, func() (port.ResolvedExecution, error) {
		return port.ResolvedExecution{OrderID: "close-1", FilledPrice: 940, FilledQuantity: 40}, nil
	})

	ok, _ := f.exec.closeOne(f.ctx, f.position, 900, "stop_loss", f.now.Add(time.Hour))
	if ok {
		t.Fatal("a partial settle fill must not book a full close")
	}
	if got := f.closedTrades(t); len(got) != 0 {
		t.Fatalf("no trade may be booked for a partial close, got %+v", got)
	}
	if st := f.statusOf(t); st != position.StatusClosing {
		t.Fatalf("position must stay CLOSING, got %q", st)
	}
}

// paper answers ClosePosition WITH a price, so the resolution path must not run
// at all: paper の測定を、live 用の修正で動かさない。
func TestCloseOne_PaperFillPath_Unchanged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(now), 0, 0)
	pb.SetPrice("7203", 1000)
	brk := newLiveCloseBroker(pb, func() (port.ResolvedExecution, error) {
		return port.ResolvedExecution{}, errors.New("must not be called on the paper path")
	})
	// Paper's own ClosePosition (with a fill price) — not the live-shaped stub.
	paperClose := &paperClosingBroker{liveCloseBroker: brk}
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	openOne(t, ctx, paperClose, posRepo, es, clock.Fixed(now), order.HoldingMultiday, order.ExecMarginSystem)
	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")

	exec := closeExecutor{broker: paperClose, posRepo: posRepo, closer: repository.NewCloser(posRepo, trades), emergency: es}
	ok, err := exec.closeOne(ctx, open[0], 900, "stop_loss", now.Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("paper close must still book immediately (ok=%v): %v", ok, err)
	}
	if brk.resolveCalls != 0 {
		t.Fatalf("ResolveExecution must not be called when the broker already reported a fill price (%d calls)", brk.resolveCalls)
	}
	list, _ := trades.ListClosedSince(ctx, now.Add(-time.Hour))
	if len(list) != 1 || list[0].ClosePrice != 1000 {
		t.Fatalf("expected 1 trade at the paper fill price 1000, got %+v", list)
	}
}

// paperClosingBroker restores paper's synchronous fill price on top of the
// live-shaped stub (keeping its ResolveExecution counter).
type paperClosingBroker struct{ *liveCloseBroker }

func (b *paperClosingBroker) ClosePosition(ctx context.Context, req port.CloseRequest) (*port.CloseResult, error) {
	return b.liveCloseBroker.Paper.ClosePosition(ctx, req)
}
