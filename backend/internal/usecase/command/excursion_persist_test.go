package command

import (
	"context"
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// 全建玉の MFE/MAE を貯める。ratchet を持たない建玉では peak が
// 一度も書かれていなかったため、非ゼロの peak は決済済み 384 本中 9 本しか無く、
// トレールの反実仮想は5分足からの再構築に頼るしかなかった。**決済規則は変えない。**
func TestManageOpenPositions_PersistsExcursionForNonRatchetPosition(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	entry := in.Summary.CurrentRate.Last
	h.broker.SetPrice("7203", entry)
	if _, err := h.cycle.Execute(ctx, in); err != nil {
		t.Fatalf("entry: %v", err)
	}
	open, _ := h.posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 || open[0].RatchetArmJPY != 0 {
		t.Fatalf("want 1 open position without a ratchet, got %+v", open)
	}
	ts := market.TickSize(entry)

	// 順行 → peak だけが動く(TP には届かない値幅)。
	up := entry + 2*ts
	h.broker.SetPrice("7203", up)
	if err := h.manage.OnTick(ctx, "7203", summaryAt("7203", up, now)); err != nil {
		t.Fatalf("manage up: %v", err)
	}
	open, _ = h.posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 {
		t.Fatalf("position must stay open: %+v", open)
	}
	wantPeak := open[0].UnrealizedJPY(up)
	if open[0].PeakUnrealizedJPY != wantPeak {
		t.Fatalf("PeakUnrealizedJPY = %v, want %v (MFE must be recorded without a ratchet)", open[0].PeakUnrealizedJPY, wantPeak)
	}

	// 逆行 → peak は残り trough が動く。
	down := entry - 2*ts
	h.broker.SetPrice("7203", down)
	if err := h.manage.OnTick(ctx, "7203", summaryAt("7203", down, now)); err != nil {
		t.Fatalf("manage down: %v", err)
	}
	open, _ = h.posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 {
		t.Fatalf("position must stay open: %+v", open)
	}
	wantTrough := open[0].UnrealizedJPY(down)
	if open[0].PeakUnrealizedJPY != wantPeak {
		t.Fatalf("PeakUnrealizedJPY = %v, want it to stay at %v", open[0].PeakUnrealizedJPY, wantPeak)
	}
	if open[0].TroughUnrealizedJPY != wantTrough {
		t.Fatalf("TroughUnrealizedJPY = %v, want %v (MAE)", open[0].TroughUnrealizedJPY, wantTrough)
	}
}

// excursionFailRepo fails only the excursion write.
type excursionFailRepo struct {
	*repository.InMemoryPositionRepo
}

func (excursionFailRepo) UpdateExcursion(context.Context, int64, float64, float64, bool) error {
	return errors.New("pg: connection reset")
}

// 戻り値を捨てていた行の回帰。書込が落ちると peak が静かに巻き戻り、arm 判定が
// やり直しになる(危険側ではないが ratchet が黙って劣化する)。
func TestManageOpenPositions_ExcursionWriteFailureSurfaces(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	entry := in.Summary.CurrentRate.Last
	h.broker.SetPrice("7203", entry)
	if _, err := h.cycle.Execute(ctx, in); err != nil {
		t.Fatalf("entry: %v", err)
	}

	var repo port.PositionRepository = excursionFailRepo{h.posRepo}
	mgr := NewManageOpenPositions(h.broker, repo, h.closer, h.emergency, clock.Fixed(now), 0)
	up := entry + 2*market.TickSize(entry)
	h.broker.SetPrice("7203", up)
	if err := mgr.OnTick(ctx, "7203", summaryAt("7203", up, now)); err == nil {
		t.Fatal("a failed excursion write must surface, not be discarded")
	}
}

func summaryAt(symbol string, price float64, now time.Time) *market.MarketSummary {
	return market.SummaryFromTicker(market.Ticker{Symbol: symbol, Bid: price, Ask: price, Last: price}, now)
}
