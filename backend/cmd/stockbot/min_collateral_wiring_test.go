package main

import (
	"log/slog"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/safety"
	"stockbot/backend/internal/testutil"
)

// 🚨 hard_limits の `margin.min_collateral_jpy` は entry 経路の snapshot まで届く。
// 以前は本番コードからの参照が 0 件で、yaml の「最低委託保証金(30万円)」は何もしていなかった。
func TestBuildSymbolBundle_WiresMinimumCollateral(t *testing.T) {
	hl := &config.HardLimits{}
	hl.Margin.MinCollateralJPY = 300_000
	d := &wiringDeps{
		broker: broker.NewPaper(time.Now, 0, 0), posRepo: repository.NewInMemoryPositionRepo(),
		tradeRepo: repository.NewInMemoryTradeRepo(), candles: repository.NewInMemoryCandleRepo(),
		pending: safety.NewPendingPositions(), emergency: safety.NewEmergencyStop(testutil.TempFlagPath(t), nil),
		engine: strategy.NewEngine(func() string { return "s" }), botCfg: &config.BotConfig{Mode: config.ModeLive},
		hardLimits: hl, logger: slog.Default(),
	}
	b := buildSymbolBundle(d, "7203", nil)
	if got := b.Cycle.MinCollateralJPY(); got != 300_000 {
		t.Fatalf("entry 経路の最低委託保証金 = %d, want 300000(hard_limits が届いていない)", got)
	}
}
