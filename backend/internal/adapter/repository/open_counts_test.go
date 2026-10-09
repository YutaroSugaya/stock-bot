package repository

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 監視銘柄の予算を数える 1 クエリぶんの集計。
//
// 🛑 **本数と銘柄数は別物**。同じ銘柄に 12 アーム乗っても監視集合は 1 銘柄なので、
// 通信量と時価の解像度に効くのは銘柄数だけ。両方を 1 回で返すのは、この集計が
// **armed 銘柄のシグナルごと**(場中は 3〜6 秒おき)に走るため — 往復を増やさない。
func TestCountOpenAcross_SeparatesPositionsFromSymbols(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryPositionRepo()
	now := time.Now()

	ins := func(sym, strat string) int64 {
		t.Helper()
		id, err := r.Insert(ctx, port.PositionInsertInput{
			Symbol: sym, Side: order.SideBuy, Quantity: 100, OpenedAt: now, StrategyName: strat,
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		return id
	}
	// 2 銘柄 / 5 建玉。7203 には入口 donchian の兄弟 2 本が乗っている。
	ins("7203", "donchian_breakout_v2")
	ins("7203", "donchian_breakout_v2_trail")
	ins("7203", "abs_momentum_v2")
	ins("6758", "donchian_breakout_v2")
	closed := ins("9984", "atr_breakout_v2")

	// CLOSED は監視から降りるので数えない。
	if err := r.MarkClosed(ctx, closed, now); err != nil {
		t.Fatalf("mark closed: %v", err)
	}

	got, err := r.CountOpenAcross(ctx, []string{"donchian_breakout_v2", "donchian_breakout_v2_trail"})
	if err != nil {
		t.Fatalf("CountOpenAcross: %v", err)
	}
	if got.Positions != 4 {
		t.Errorf("Positions = %d, want 4", got.Positions)
	}
	if got.Symbols != 2 {
		t.Errorf("Symbols = %d, want 2(7203 / 6758。同じ銘柄の複数アームは 1 銘柄)", got.Symbols)
	}
	// 入口 donchian は 7203(兄弟 2 本)と 6758 の **2 銘柄**。3 建玉だが枠は銘柄で数える。
	if got.EntryArmSymbols != 2 {
		t.Errorf("EntryArmSymbols = %d, want 2", got.EntryArmSymbols)
	}
}

// 入口名を渡さなければ入口の集計は 0(枠を使わない呼び手のため)。
func TestCountOpenAcross_NoEntryArmsMeansZero(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryPositionRepo()
	if _, err := r.Insert(ctx, port.PositionInsertInput{
		Symbol: "7203", Side: order.SideBuy, Quantity: 100, OpenedAt: time.Now(), StrategyName: "donchian_breakout_v2",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := r.CountOpenAcross(ctx, nil)
	if err != nil {
		t.Fatalf("CountOpenAcross: %v", err)
	}
	if got.Symbols != 1 || got.EntryArmSymbols != 0 {
		t.Fatalf("Symbols=%d EntryArmSymbols=%d, want 1 / 0", got.Symbols, got.EntryArmSymbols)
	}
}

// 既存の CountOpenAllSymbols と食い違わないこと(呼び手が 2 系統あるので、
// 片方だけ直すと枠の判断と画面の表示が静かにずれる)。
func TestCountOpenAcross_AgreesWithCountOpenAllSymbols(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryPositionRepo()
	now := time.Now()
	for _, sym := range []string{"7203", "7203", "6758"} {
		if _, err := r.Insert(ctx, port.PositionInsertInput{
			Symbol: sym, Side: order.SideBuy, Quantity: 100, OpenedAt: now, StrategyName: "abs_momentum_v2",
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	n, err := r.CountOpenAllSymbols(ctx)
	if err != nil {
		t.Fatalf("CountOpenAllSymbols: %v", err)
	}
	got, err := r.CountOpenAcross(ctx, nil)
	if err != nil {
		t.Fatalf("CountOpenAcross: %v", err)
	}
	if got.Positions != n {
		t.Fatalf("Positions = %d, CountOpenAllSymbols = %d — 2 つの集計がずれている", got.Positions, n)
	}
}
