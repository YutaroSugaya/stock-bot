package command

import (
	"context"
	"fmt"
	"path/filepath"
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

// asyncSettleStub は「決済の約定が同期に返らない」broker(立花)を模す。closeOne が
// 塞いだ穴と同じものが reconcile の再送枝にも残っていたので、両方をこの stub で突く。
type asyncSettleStub struct {
	*broker.Paper
	closeOrders map[string]bool
	orderID     string // ClosePosition が返す注文番号("" = 受理されたが番号が返らない)
	outcome     func() (port.ResolvedExecution, error)
	hideOrders  bool // true = 板に注文が 1 本も無い(翌朝)
	// restingOrders は板に載っている注文を明示する(nil = Paper に委譲・既定)。
	// 成行の返済注文は逆指値脚を持たないので、Paper の守り注文では代用できない。
	restingOrders []order.Order
}

func newAsyncSettleStub(p *broker.Paper, orderID string, outcome func() (port.ResolvedExecution, error)) *asyncSettleStub {
	return &asyncSettleStub{Paper: p, closeOrders: map[string]bool{}, orderID: orderID, outcome: outcome}
}

func (b *asyncSettleStub) SettleFillsAsync() bool { return true }

func (b *asyncSettleStub) ClosePosition(_ context.Context, _ port.CloseRequest) (*port.CloseResult, error) {
	if b.orderID != "" {
		b.closeOrders[b.orderID] = true
	}
	return &port.CloseResult{OrderID: b.orderID, Accepted: true, FilledPrice: 0}, nil
}

// 翌朝を模す: 当日限りの決済注文は板から消え、守りの脚は決済前に cancel 済み。
// (paper の CancelOrder は板から消さない no-op なので、ここで明示する)
func (b *asyncSettleStub) GetActiveOrders(ctx context.Context, symbol string) ([]order.Order, error) {
	if b.hideOrders {
		return nil, nil
	}
	if b.restingOrders != nil {
		return b.restingOrders, nil
	}
	return b.Paper.GetActiveOrders(ctx, symbol)
}

func (b *asyncSettleStub) ResolveExecution(ctx context.Context, orderID string) (port.ResolvedExecution, error) {
	if b.closeOrders[orderID] {
		return b.outcome()
	}
	return b.Paper.ResolveExecution(ctx, orderID)
}

func notFilled() (port.ResolvedExecution, error) {
	return port.ResolvedExecution{}, fmt.Errorf("settle: %w", port.ErrOrderNotFilled)
}

// 🚨 closeOne で塞いだ幽霊決済が reconcile の再送枝(resolveStuckClosing)に丸ごと
// 残っていた。未約定の決済注文は当日限りで消えるので、**翌営業日の reconcile は
// 必ずこの枝に入る** — つまり穴は塞がっておらず経路が 1 日ずれただけだった。
func TestReconcile_StuckClosingResendDoesNotBookUnfilledClose(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(now), 0, 0)
	pb.SetPrice("7203", 1000)
	brk := newAsyncSettleStub(pb, "close-1", notFilled)
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)

	openOne(t, ctx, brk, posRepo, es, clock.Fixed(now), order.HoldingMultiday, order.ExecMarginSystem)
	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	closer := repository.NewCloser(posRepo, trades)
	exec := closeExecutor{broker: brk, posRepo: posRepo, closer: closer, emergency: es}
	if ok, _ := exec.closeOne(ctx, open[0], 900, "stop_loss", now); ok {
		t.Fatal("precondition: the settle must not fill")
	}
	brk.hideOrders = true // 当日限りの決済注文が消えた翌朝

	rec := NewReconcile(brk, posRepo, closer, safety.NewPendingPositions(), es, clock.Fixed(now))
	rec.gracePeriod = 0 // 猶予を超えた状態(翌朝)
	if _, err := rec.Run(ctx, "7203"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	list, _ := trades.ListClosedSince(ctx, now.Add(-time.Hour))
	if len(list) != 0 {
		t.Fatalf("reconcile must not book a close it could not confirm, got %+v", list)
	}
	after, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	if len(after) != 1 || after[0].Status != position.StatusClosing {
		t.Fatalf("position must stay CLOSING, got %+v", after)
	}
}

// 「照会できない broker」と「照会できるのに注文番号が返らなかった」を畳んではいけない。
// 立花の accepted() は sOrderNumber を見ないので、後者は live で実在しうる。
func TestCloseOne_AcceptedWithoutOrderID_BooksNothing(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(now), 0, 0)
	pb.SetPrice("7203", 1000)
	brk := newAsyncSettleStub(pb, "", notFilled) // 受理されたが注文番号が空
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)

	openOne(t, ctx, brk, posRepo, es, clock.Fixed(now), order.HoldingMultiday, order.ExecMarginSystem)
	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")

	exec := closeExecutor{broker: brk, posRepo: posRepo, closer: repository.NewCloser(posRepo, trades), emergency: es}
	if ok, _ := exec.closeOne(ctx, open[0], 900, "stop_loss", now); ok {
		t.Fatal("an unresolvable settle (no order id) must not be booked")
	}
	if list, _ := trades.ListClosedSince(ctx, now.Add(-time.Hour)); len(list) != 0 {
		t.Fatalf("no trade may be booked, got %+v", list)
	}
}

// paper は決済の約定を同期に返す。**価格を持たない銘柄でも**新しい解決経路に入っては
// いけない — 再起動後に当日ずっと stale な銘柄(売買停止・寄らず)は `p.prices` が
// 空のままで、そこが経路に入ると paper の測定が黙って変わる(手仕舞いが trip する)。
func TestCloseOne_PaperWithoutPrice_KeepsLegacyObservedPricePath(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(now), 0, 0)
	pb.SetPrice("7203", 1000)
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	openOne(t, ctx, pb, posRepo, es, clock.Fixed(now), order.HoldingMultiday, order.ExecMarginSystem)
	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")

	// 再起動後を模す: 建玉だけ台帳から復元され、価格はまだ 1 本も来ていない。
	restarted := broker.NewPaper(clock.Fixed(now), 0, 0)
	restarted.AdoptOpenPositions([]port.BrokerPosition{{
		BrokerPositionID: open[0].BrokerPositionID, Symbol: "7203",
		Side: open[0].Side, Quantity: open[0].Quantity, EntryPrice: open[0].EntryPrice,
	}})

	exec := closeExecutor{broker: restarted, posRepo: posRepo, closer: repository.NewCloser(posRepo, trades), emergency: es}
	ok, err := exec.closeOne(ctx, open[0], 900, "manual", now)
	if err != nil || !ok {
		t.Fatalf("paper must still book at the observed price (ok=%v): %v", ok, err)
	}
	list, _ := trades.ListClosedSince(ctx, now.Add(-time.Hour))
	if len(list) != 1 || list[0].ClosePrice != 900 {
		t.Fatalf("want 1 trade at the observed 900, got %+v", list)
	}
}

// 計測器の書込失敗が守りのループを止めてはいけない。error は surface するが、
// **その tick の他の建玉の決済判定は最後まで走らせる**。
func TestManageOpenPositions_ExcursionWriteFailureStillEvaluatesOtherExits(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	entry := in.Summary.CurrentRate.Last
	h.broker.SetPrice("7203", entry)
	if _, err := h.cycle.Execute(ctx, in); err != nil {
		t.Fatalf("entry: %v", err)
	}
	// 同一銘柄にもう 1 本。1 本目の記録書込が落ちても 2 本目の TP は見る。
	open, _ := h.posRepo.ListOpenOrClosing(ctx, "7203")
	second, err := h.posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "bp-second", Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		EntryPrice: entry, TakeProfitPrice: entry + 1, StopLossPrice: entry - 500,
		StrategyConfigID: open[0].StrategyConfigID, HoldingMode: order.HoldingMultiday,
		ExecKind: order.ExecMarginSystem, OpenedAt: now,
	})
	if err != nil {
		t.Fatalf("insert second: %v", err)
	}

	var repo port.PositionRepository = excursionFailRepo{h.posRepo}
	mgr := NewManageOpenPositions(h.broker, repo, h.closer, h.emergency, clock.Fixed(now), 0)
	up := entry + 2*market.TickSize(entry)
	h.broker.SetPrice("7203", up)
	if err := mgr.OnTick(ctx, "7203", summaryAt("7203", up, now)); err == nil {
		t.Fatal("a failed excursion write must still surface")
	}
	p2, _ := h.posRepo.GetByID(ctx, second)
	if p2 == nil || p2.Status == position.StatusOpen {
		t.Fatalf("the second position's take-profit must still be evaluated in the same tick, got %+v", p2)
	}
}

// 場中のケース(決済注文が板に残っている間)。翌朝ケースと違って再送はせず、
// **裸のまま滞留していることを数える**だけ。ここが B4 の主目的で、報告を捨てて
// いた頃はログにも画面にも出ていなかった。
func TestReconcile_CountsStuckClosingWhileSettleOrderRests(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(now), 0, 0)
	pb.SetPrice("7203", 1000)
	brk := newAsyncSettleStub(pb, "close-1", notFilled)
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)

	openOne(t, ctx, brk, posRepo, es, clock.Fixed(now), order.HoldingMultiday, order.ExecMarginSystem)
	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	closer := repository.NewCloser(posRepo, trades)
	exec := closeExecutor{broker: brk, posRepo: posRepo, closer: closer, emergency: es}
	if ok, _ := exec.closeOne(ctx, open[0], 900, "stop_loss", now); ok {
		t.Fatal("precondition: the settle must not fill")
	}

	rec := NewReconcile(brk, posRepo, closer, safety.NewPendingPositions(), es, clock.Fixed(now))
	rec.gracePeriod = 0
	rep, err := rec.Run(ctx, "7203") // 板に決済注文が残ったまま(hideOrders=false)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.StuckClosing != 1 {
		t.Fatalf("StuckClosing = %d, want 1(守りの脚は cancel 済み = 裸で滞留している)", rep.StuckClosing)
	}
	if list, _ := trades.ListClosedSince(ctx, now.Add(-time.Hour)); len(list) != 0 {
		t.Fatalf("nothing may be booked while the settle rests, got %+v", list)
	}
}
