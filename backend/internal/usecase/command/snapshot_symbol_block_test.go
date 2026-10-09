package command

import (
	"context"
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

type stubBlocks struct {
	blocks []port.SymbolBlock
	err    error
}

func (s stubBlocks) List(context.Context) ([]port.SymbolBlock, error) { return s.blocks, s.err }

func symbolBlockSnapshot(t *testing.T, sym string, r port.SymbolBlockReader) risk.AccountSnapshot {
	t.Helper()
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	sb := NewSnapshotBuilder(h.posRepo, h.tradeRepo, h.broker, h.emergency, h.hours, clock.Fixed(now),
		SnapshotCaps{WindowMinutes: 60})
	if r != nil {
		sb = sb.WithSymbolBlocks(r)
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: sym,
		EntryPrice: 2500, StopLossJPY: 50, Quantity: 100, HoldingMode: order.HoldingMultiday}
	return sb.BuildStructural(context.Background(), sym, &market.MarketSummary{}, sig, order.ExecMarginSystem)
}

func TestBuildStructural_FillsManualSymbolBlock(t *testing.T) {
	r := stubBlocks{blocks: []port.SymbolBlock{{Symbol: "6594"}}}
	if s := symbolBlockSnapshot(t, "6594", r); !s.ManualSymbolBlocked || s.SymbolBlocksUnreadable {
		t.Fatalf("止めた銘柄: blocked=%v unreadable=%v", s.ManualSymbolBlocked, s.SymbolBlocksUnreadable)
	}
	if s := symbolBlockSnapshot(t, "7203", r); s.ManualSymbolBlocked {
		t.Fatal("止めていない銘柄まで止まった")
	}
}

func TestBuildStructural_UnreadableBlocksFailClose(t *testing.T) {
	s := symbolBlockSnapshot(t, "7203", stubBlocks{err: errors.New("broken")})
	if !s.SymbolBlocksUnreadable {
		t.Fatal("停止のファイルが読めないのに新規を止めない(fail-open)")
	}
}

// research(store を配線しない track)には効かない。
func TestBuildStructural_NoStoreMeansNoBlock(t *testing.T) {
	if s := symbolBlockSnapshot(t, "6594", nil); s.ManualSymbolBlocked || s.SymbolBlocksUnreadable {
		t.Fatalf("store 無しで止まった: %+v", s)
	}
}
