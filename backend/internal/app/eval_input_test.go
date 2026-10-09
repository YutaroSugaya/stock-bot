package app

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
)

// 🛑 EvalInput.Hours を渡し忘れると、戦略別 MaxHold が**祝日を知らない**営業日に
// 静かに縮退する(エラーも警告も出ない)。EvalInput の組み立てをメソッドに切り出して
// あるのは、この 1 行を回帰テストで押さえるため。
func TestSymbolBundleEvalInputCarriesTradingCalendar(t *testing.T) {
	now := time.Date(2026, 8, 24, 9, 30, 0, 0, tokyoHours().TZ)
	b := &SymbolBundle{
		Symbol: "7203", Hours: tokyoHours(), Agg: market.NewAggregator("7203", 64),
		Clock: func() time.Time { return now },
		// 日足の取得経路は本テストの対象外(broker を持たせない)。
		AllowHistoryFetch: func(time.Time) bool { return false },
	}
	cfg := &config.StrategyConfig{Symbol: "7203", StrategyName: config.StrategyNoTrade}
	sum := &market.MarketSummary{Symbol: "7203", CurrentRate: market.CurrentRate{Last: 2000}}

	in := b.evalInput(context.Background(), now, sum, cfg)

	if in.Hours.TZ == nil {
		t.Fatal("EvalInput.Hours が空 — 戦略別 MaxHold が祝日を無視する営業日に縮退する")
	}
	if in.Hours.TZ.String() != tokyoHours().TZ.String() {
		t.Fatalf("Hours.TZ = %q, want %q", in.Hours.TZ, tokyoHours().TZ)
	}
	if !in.Now.Equal(now) || in.Config != cfg || in.Summary != sum {
		t.Fatal("evalInput が now / config / summary をそのまま渡していない")
	}
}
