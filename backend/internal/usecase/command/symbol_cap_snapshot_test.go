package command

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
)

// 監視銘柄の予算を gate へ運ぶ配線。ここを埋め忘れると
// 枠が**常に 0 = 無効**になり、config に値を書いても 1 度も効かない(fail-open)。
func newSymbolCapBuilder(t *testing.T, posRepo port.PositionRepository, now time.Time, caps SnapshotCaps) *SnapshotBuilder {
	t.Helper()
	c := clock.Fixed(now)
	return NewSnapshotBuilder(posRepo, repository.NewInMemoryTradeRepo(), broker.NewPaper(c, 1, 0),
		safety.NewEmergencyStop(t.TempDir()+"/flag", nil),
		tokyoHours(), c, caps)
}

func TestBuildStructural_FillsSymbolBudget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, clock.JST)
	posRepo := repository.NewInMemoryPositionRepo()

	ins := func(sym, strat string) {
		t.Helper()
		if _, err := posRepo.Insert(ctx, port.PositionInsertInput{
			Symbol: sym, Side: order.SideBuy, Quantity: 100, OpenedAt: now, StrategyName: strat,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// 3 銘柄 / 4 建玉。7203 には入口 donchian の兄弟 2 本が乗っている。
	ins("7203", string(config.StrategyDonchianBreakoutV2))
	ins("7203", string(config.StrategyDonchianBreakoutV2Trail))
	ins("6758", string(config.StrategyDonchianBreakoutV2))
	ins("9984", string(config.StrategyAbsMomentumV2))

	caps := SnapshotCaps{AccountMaxOpenSymbols: 80, EntryArmMaxOpenSymbols: 12}
	b := newSymbolCapBuilder(t, posRepo, now, caps)

	// まだ持っていない銘柄で donchian_trail のシグナル。
	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Symbol: "4063", Side: order.SideBuy, Quantity: 100,
		StrategyName: config.StrategyDonchianBreakoutV2Trail,
	}
	snap := b.BuildStructural(ctx, "4063", nil, sig, order.ExecMarginGeneral)
	if snap.AccountMaxOpenSymbols != 80 || snap.EntryArmMaxOpenSymbols != 12 {
		t.Fatalf("cap が gate へ渡っていない: account=%d entry=%d", snap.AccountMaxOpenSymbols, snap.EntryArmMaxOpenSymbols)
	}
	if snap.AccountOpenSymbols != 3 {
		t.Errorf("AccountOpenSymbols = %d, want 3(建玉 4 本でも銘柄は 3)", snap.AccountOpenSymbols)
	}
	if snap.AccountOpenPositions != 4 {
		t.Errorf("AccountOpenPositions = %d, want 4", snap.AccountOpenPositions)
	}
	// 入口 donchian は 7203 と 6758 の 2 銘柄(兄弟 2 本は 1 銘柄)。
	if snap.EntryArmOpenSymbols != 2 {
		t.Errorf("EntryArmOpenSymbols = %d, want 2", snap.EntryArmOpenSymbols)
	}
	if snap.SymbolAlreadyHeld || snap.EntryArmHoldsSymbol {
		t.Errorf("4063 はまだ持っていない: held=%v armHeld=%v", snap.SymbolAlreadyHeld, snap.EntryArmHoldsSymbol)
	}

	// 既に兄弟が持っている銘柄なら、両方の「保有中」フラグが立って枠を消費しない。
	sig.Symbol = "7203"
	snap = b.BuildStructural(ctx, "7203", nil, sig, order.ExecMarginGeneral)
	if !snap.SymbolAlreadyHeld {
		t.Error("SymbolAlreadyHeld が立っていない — 監視済みの銘柄が全体の枠を食う")
	}
	if !snap.EntryArmHoldsSymbol {
		t.Error("EntryArmHoldsSymbol が立っていない — ペアの 2 本目が枠の境界で弾かれる")
	}

	// 別の入口(abs)が持っているだけの銘柄では、入口のフラグは立たない
	// (全体のフラグは立つ = 監視済みなので通信コストはゼロ)。
	sig.Symbol = "9984"
	snap = b.BuildStructural(ctx, "9984", nil, sig, order.ExecMarginGeneral)
	if !snap.SymbolAlreadyHeld {
		t.Error("9984 は監視済み(abs が保有)なので SymbolAlreadyHeld は立つ")
	}
	if snap.EntryArmHoldsSymbol {
		t.Error("入口が違うのに EntryArmHoldsSymbol が立っている — 入口の予約枠が素通しになる")
	}
}
