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

// bot_config の `risk.account_max_entries_per_day` は entry 経路の snapshot まで届く。
// 配線を忘れると「yaml に書いたのに一度も効かない」= 一番気づきにくい壊れ方になる
// (min_collateral_jpy も以前はその状態だった)。
func TestBuildSymbolBundle_WiresAccountEntriesPerDayCap(t *testing.T) {
	bc := &config.BotConfig{Mode: config.ModeLive}
	bc.Risk.AccountMaxEntriesPerDay = 2
	d := &wiringDeps{
		broker: broker.NewPaper(time.Now, 0, 0), posRepo: repository.NewInMemoryPositionRepo(),
		tradeRepo: repository.NewInMemoryTradeRepo(), candles: repository.NewInMemoryCandleRepo(),
		pending: safety.NewPendingPositions(), emergency: safety.NewEmergencyStop(testutil.TempFlagPath(t), nil),
		engine: strategy.NewEngine(func() string { return "s" }), botCfg: bc,
		hardLimits: &config.HardLimits{}, logger: slog.Default(),
	}
	b := buildSymbolBundle(d, "7203", nil)
	if got := b.Cycle.AccountMaxEntriesPerDay(); got != 2 {
		t.Fatalf("entry 経路の 1 日の新規本数の上限 = %d, want 2(bot_config が届いていない)", got)
	}
}
