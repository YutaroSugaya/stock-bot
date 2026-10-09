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

// 🚨 口座単位の entry 排他は**トラックに 1 つ**。銘柄ごとに作ると
// 排他が何も排他しない(= 2 銘柄が同じ空き枠を取る TOCTOU が残る)。挿し忘れも同じ。
func TestBuildSymbolBundle_SharesOneEntryLockPerTrack(t *testing.T) {
	d := &wiringDeps{
		broker: broker.NewPaper(time.Now, 0, 0), posRepo: repository.NewInMemoryPositionRepo(),
		tradeRepo: repository.NewInMemoryTradeRepo(), candles: repository.NewInMemoryCandleRepo(),
		pending: safety.NewPendingPositions(), emergency: safety.NewEmergencyStop(testutil.TempFlagPath(t), nil),
		engine: strategy.NewEngine(func() string { return "s" }), botCfg: &config.BotConfig{Mode: config.ModePaper},
		hardLimits: &config.HardLimits{}, logger: slog.Default(),
	}
	a := buildSymbolBundle(d, "7203", nil)
	b := buildSymbolBundle(d, "6758", nil)
	if a.Cycle.EntryLock == nil {
		t.Fatal("entry の排他が配線されていない")
	}
	if a.Cycle.EntryLock != b.Cycle.EntryLock {
		t.Fatal("銘柄ごとに別の排他 = 口座の枠を直列化していない")
	}
}
