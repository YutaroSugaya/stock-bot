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
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
)

type lowMarginBroker struct {
	*broker.Paper
	ratio float64
}

func (b *lowMarginBroker) GetAccountMargin(context.Context) (*order.AccountMargin, error) {
	return &order.AccountMargin{AvailableJPY: 100000, MarginRatio: b.ratio, Equity: 100000}, nil
}

// CheckMaintenance must fire regardless of quote health — it is price-independent.
func TestCheckMaintenance_TripsOnBreach(t *testing.T) {
	ctx := context.Background()
	c := clock.Fixed(time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST))
	pb := broker.NewPaper(c, 0, 0)
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	brk := &lowMarginBroker{Paper: pb, ratio: 0.20} // below the 0.30 floor

	mgr := NewManageOpenPositions(brk, repository.NewInMemoryPositionRepo(), nil, es, c, 0.30)
	mgr.CheckMaintenance(ctx)
	if !es.Active() {
		t.Fatal("a 維持率 breach must trip emergency")
	}
}

func TestCheckMaintenance_HealthyRatioDoesNotTrip(t *testing.T) {
	ctx := context.Background()
	c := clock.Fixed(time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST))
	pb := broker.NewPaper(c, 0, 0)
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	brk := &lowMarginBroker{Paper: pb, ratio: 0.80} // healthy

	mgr := NewManageOpenPositions(brk, repository.NewInMemoryPositionRepo(), nil, es, c, 0.30)
	mgr.CheckMaintenance(ctx)
	if es.Active() {
		t.Fatal("a healthy 維持率 must not trip")
	}
}

var _ port.Broker = (*lowMarginBroker)(nil)

// 🛑 **維持率ブレーカーは間引かない**(口座を見に行くことが目的)。同時に、その実照会は
// entry 経路のキャッシュも温める —— 保有中は 1 時間に 1 回ブレーカーが回るので、
// entry 側は追加の wire を 1 回も払わずに新しい値を読める。
func TestCheckMaintenance_AlwaysQueriesAndWarmsTheEntryCache(t *testing.T) {
	ctx := context.Background()
	c := clock.Fixed(time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST))
	pb := broker.NewPaper(c, 0, 0)
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	cb := &countingBroker{Paper: pb}
	cache := NewAccountMarginCache(cb, time.Hour, c)

	mgr := NewManageOpenPositions(cb, repository.NewInMemoryPositionRepo(), nil, es, c, 0.30).
		WithAccountMargin(cache)
	mgr.CheckMaintenance(ctx)
	mgr.CheckMaintenance(ctx)
	if got := cb.marginCalls.Load(); got != 2 {
		t.Fatalf("ブレーカーの照会が %d 回 (want 2 — ここは間引かない)", got)
	}
	if _, ok := cache.Get(ctx); !ok {
		t.Fatal("ブレーカーの照会が entry 経路のキャッシュを温めていない")
	}
	if got := cb.marginCalls.Load(); got != 2 {
		t.Fatalf("温まったキャッシュを読んだのに追加照会が飛んだ (calls=%d)", got)
	}
}
