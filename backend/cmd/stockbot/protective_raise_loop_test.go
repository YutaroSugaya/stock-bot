package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/command"
)

// raiseBoard は板の守りの最小の偽物(照会・取消・再発注)。
type raiseBoard struct {
	orders    []port.ProtectiveOrderInfo
	cancelled []string
	placed    []port.OCOCloseOrderInput
}

func (b *raiseBoard) ListProtectiveOrders(_ context.Context, sym string) ([]port.ProtectiveOrderInfo, error) {
	var out []port.ProtectiveOrderInfo
	for _, o := range b.orders {
		if o.Symbol == sym {
			out = append(out, o)
		}
	}
	return out, nil
}
func (b *raiseBoard) CancelProtectiveOrder(_ context.Context, o port.ProtectiveOrderInfo) error {
	b.cancelled = append(b.cancelled, o.OrderID)
	return nil
}
func (b *raiseBoard) PlaceSettleOCO(_ context.Context, in port.OCOCloseOrderInput) (string, error) {
	b.placed = append(b.placed, in)
	return "new", nil
}

type noTrip struct{}

func (noTrip) Trip(string, time.Time) error { return nil }
func (noTrip) Active() bool                 { return false }

// 🚨 寄り前の守りの手当ては **期日の置き直し → 線への引き上げ** の順に 1 本の流れで回す。
// 別々の goroutine にすると同じ銘柄で取消 → 再発注が重なる。
func liveWithArmedTrail(t *testing.T, now time.Time) (*liveTrack, *raiseBoard) {
	t.Helper()
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	id, err := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "b-5726", Symbol: "5726", Side: order.SideBuy, Quantity: 100, EntryPrice: 2000,
		OpenedAt: time.Date(2026, 9, 30, 9, 0, 0, 0, clock.JST), HoldingMode: order.HoldingMultiday,
		ExecKind: order.ExecMarginSystem, StrategyName: "bnf_reversion_trail", StopLossPrice: 1800, StopLossJPY: 200,
		RatchetArmJPY: 100, RatchetGivebackJPY: 150, RatchetFloorAtArm: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateExcursion(ctx, id, 180, -20, true); err != nil {
		t.Fatal(err)
	}
	board := &raiseBoard{orders: []port.ProtectiveOrderInfo{{
		OrderID: "o-5726", Symbol: "5726", HasStopLeg: true, Side: order.SideSell, Quantity: 100,
		ExpireOn: time.Date(2026, 10, 9, 0, 0, 0, 0, clock.JST), StopTrigger: 1800,
	}}}
	h := replaceLoopHours()
	clk := func() time.Time { return now }
	ref := func(context.Context, string) float64 { return 2150 }
	lt := &liveTrack{hours: h, clock: clk}
	lt.replaceProtective = command.NewReplaceProtectiveOrder(repo, board, h, clk, noTrip{}).WithPriceLimitRef(ref)
	lt.raiseTrailStops = command.NewRaiseTrailStops(repo, board, h, clk,
		command.NewRepriceProtectiveOrder(repo, board, h, clk, noTrip{}).WithPriceLimitRef(ref)).WithPriceLimitRef(ref)
	return lt, board
}

func TestPreOpenRunRaisesArmedTrailStopsAfterTheExpiryPass(t *testing.T) {
	lt, board := liveWithArmedTrail(t, time.Date(2026, 10, 5, 8, 0, 0, 0, clock.JST))
	runReplaceProtectiveExpiry(context.Background(), lt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(board.placed) != 1 || board.placed[0].StopLoss != 2100 {
		t.Fatalf("placed=%+v — 寄り前の流れで線(2100)へ引き上げていない", board.placed)
	}
}

func TestPreOpenRunDoesNothingDuringTheSession(t *testing.T) {
	lt, board := liveWithArmedTrail(t, time.Date(2026, 10, 5, 10, 30, 0, 0, clock.JST))
	runReplaceProtectiveExpiry(context.Background(), lt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(board.cancelled) != 0 || len(board.placed) != 0 {
		t.Fatalf("場中に守りを触った: cancelled=%v placed=%v", board.cancelled, board.placed)
	}
}

// 🛑 守りの自動復旧(rearm)は、寄り前の置き直し・引き上げが走っている間は待つ。取消と再発注の間に
// 復旧が「守りが無い」と読んで置くと、再発注が拒否されて emergency が trip する。
func TestRearmWaitsWhileThePreOpenPassHoldsTheProtectiveLock(t *testing.T) {
	lt := &liveTrack{}
	lt.protectiveMu.Lock()
	done := make(chan struct{})
	go func() {
		runRearmProtective(context.Background(), lt, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("寄り前の手当てが鍵を持っている間に rearm が走った")
	case <-time.After(50 * time.Millisecond):
	}
	lt.protectiveMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("鍵を返しても rearm が終わらない")
	}
}
