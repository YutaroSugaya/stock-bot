package repository

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

func dbar(sym string, close float64, openTime time.Time) market.Candle {
	return market.Candle{Symbol: sym, Interval: 24 * time.Hour, OpenTime: openTime, Open: close, High: close, Low: close, Close: close}
}

// Upsert must DEDUP by open_time (ON CONFLICT DO NOTHING), so a periodic live
// refresh that re-fetches overlapping days does not duplicate bars — and the
// series stays chronological.
func TestInMemoryCandleRepo_UpsertDedupsAndOrders(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryCandleRepo()
	base := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

	// initial seed: day0, day1
	_ = r.Upsert(ctx, "7203", []market.Candle{dbar("7203", 100, base), dbar("7203", 101, base.AddDate(0, 0, 1))})
	// refresh: overlapping day1 (上書きされる) + new day2, delivered out of order
	_ = r.Upsert(ctx, "7203", []market.Candle{dbar("7203", 102, base.AddDate(0, 0, 2)), dbar("7203", 999, base.AddDate(0, 0, 1))})

	got, err := r.List(ctx, "7203", port.PeriodDaily, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 distinct bars (dedup by open_time), got %d: %+v", len(got), got)
	}
	// chronological
	for i := 1; i < len(got); i++ {
		if !got[i-1].OpenTime.Before(got[i].OpenTime) {
			t.Fatalf("bars must be chronological, got %+v", got)
		}
	}
	// 🛑 同一 open_time は **上書き**(DO NOTHING から変更)。
	// 旧規約は「リフレッシュが履歴を壊さない」ためだったが、**株式分割は過去のバーを
	// 遡って書き換える正当な操作**で、DO NOTHING だと chain-link 済みの調整値が永久に
	// 入らなかった(1:4 分割の銘柄で実害 — 偽の -65% 暴落を BNF が live で arm した)。
	// 壊れた broker バーから守るのは repo ではなく上流の discontinuityVs。
	if got[1].Close != 999 {
		t.Fatalf("conflict must OVERWRITE with the refreshed bar (999), got %.0f", got[1].Close)
	}
}
