package command

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/safety"
)

// FeeJPY was once never written, so the ledger's "net" silently degraded to gross.
func TestClose_RecordsRoundTripFeeSoDailyLossIsNet(t *testing.T) {
	ctx := context.Background()
	openAt := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)

	const feePerLeg = 500.0
	pb := broker.NewPaper(clock.Fixed(openAt), 0, feePerLeg) // fee on entry AND close
	pb.SetPrice("7203", 1000)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	closer := repository.NewCloser(posRepo, tradeRepo)
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)

	openOne(t, ctx, pb, posRepo, es, clock.Fixed(openAt), order.HoldingIntraday, order.ExecMarginOneday)
	mgr := NewManageOpenPositions(pb, posRepo, closer, es, clock.Fixed(openAt.Add(time.Hour)), 0)

	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 {
		t.Fatalf("want 1 open position, got %d", len(open))
	}
	if ok, err := mgr.exec.closeOne(ctx, open[0], 1000, "manual", openAt.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("close: ok=%v err=%v", ok, err)
	}

	startOfDay := time.Date(2026, 6, 17, 0, 0, 0, 0, clock.JST)
	loss, err := tradeRepo.SumClosedLossJPYSince(ctx, startOfDay)
	if err != nil {
		t.Fatal(err)
	}
	if loss != int(2*feePerLeg) {
		t.Fatalf("net daily loss = %d, want %d (round-trip fee) — fee not written to the trade row", loss, int(2*feePerLeg))
	}
}

// CarryJPY が 0 のままだと multiday 保有の net が金利分だけ甘く出て forward の
// エッジ判定が嘘になる。台帳の規約は net = gross − fee + carry なので carry は負値。
func TestClose_RecordsMarginCarrySoNetIncludesInterest(t *testing.T) {
	ctx := context.Background()
	openAt := time.Date(2026, 7, 24, 13, 0, 0, 0, clock.JST)  // 金曜
	closeAt := time.Date(2026, 7, 27, 10, 0, 0, 0, clock.JST) // 月曜

	pb := broker.NewPaper(clock.Fixed(openAt), 0, 0)
	pb.SetPrice("7203", 3000)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()
	closer := repository.NewCloser(posRepo, tradeRepo)
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)

	openOne(t, ctx, pb, posRepo, es, clock.Fixed(openAt), order.HoldingMultiday, order.ExecMarginGeneral)
	mgr := NewManageOpenPositions(pb, posRepo, closer, es, clock.Fixed(closeAt), 0).
		WithCarry(position.CarryCalc{
			Rates: position.MarginRates{BuyAnnualRate: 0.025, SellAnnualRate: 0.0115},
			TZ:    clock.JST,
			IsTradingDay: func(d time.Time) bool {
				w := d.In(clock.JST).Weekday()
				return w != time.Saturday && w != time.Sunday
			},
			SettlementDays: 2,
		})

	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 {
		t.Fatalf("want 1 open position, got %d", len(open))
	}
	if ok, err := mgr.exec.closeOne(ctx, open[0], 3000, "manual", closeAt); err != nil || !ok {
		t.Fatalf("close: ok=%v err=%v", ok, err)
	}

	trades, err := tradeRepo.ListClosedSince(ctx, time.Time{})
	if err != nil || len(trades) != 1 {
		t.Fatalf("ListClosedSince: %v (n=%d)", err, len(trades))
	}
	// 受渡 7/28(火) → 7/29(水) の両端入れ = 2 日。300,000 × 2.5% ÷ 365 × 2
	want := -300000 * 0.025 / 365 * 2
	if diff := trades[0].CarryJPY - want; diff > 0.001 || diff < -0.001 {
		t.Fatalf("CarryJPY = %v, want %v (一般信用 買方金利 2 日分)", trades[0].CarryJPY, want)
	}
}
