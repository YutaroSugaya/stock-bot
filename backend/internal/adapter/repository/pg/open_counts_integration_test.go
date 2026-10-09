//go:build integration

// `CountOpenAcross` の **SQL** を実 Postgres で固定する。in-memory 実装のテストは
// 同じ規則の別実装を検査するだけで、`count(DISTINCT symbol) FILTER (...)` の綴りは
// 守れない —— ここが壊れると入口の枠が**常に 0 = 無効**になり(fail-open)、
// config に値を書いても 1 度も効かないまま緑で通る。
package pg

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

func TestPg_CountOpenAcross(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()

	cfgRepo := NewConfigRepo(pool)
	if err := cfgRepo.EnsureExists(ctx, port.StrategyConfigRecord{
		ConfigID: "cfg-counts", Symbol: "7203", Mode: "paper_config", StrategyName: "no_trade", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	posRepo := NewPositionRepo(pool)
	ins := func(sym, strat string) int64 {
		t.Helper()
		id, err := posRepo.Insert(ctx, port.PositionInsertInput{
			Symbol: sym, Side: order.SideBuy, Quantity: 100, EntryPrice: 2500,
			StrategyConfigID: "cfg-counts", StrategyName: strat,
			HoldingMode: order.HoldingMultiday, ExecKind: order.ExecMarginSystem, OpenedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("insert %s/%s: %v", sym, strat, err)
		}
		return id
	}
	// 3 銘柄 / 5 建玉。7203 には入口 donchian の兄弟 2 本が乗っている。
	ins("7203", "donchian_breakout_v2")
	ins("7203", "donchian_breakout_v2_trail")
	ins("7203", "abs_momentum_v2")
	ins("6758", "donchian_breakout_v2")
	closed := ins("9984", "atr_breakout_v2")
	if err := posRepo.MarkClosed(ctx, closed, time.Now()); err != nil {
		t.Fatalf("mark closed: %v", err)
	}

	got, err := posRepo.CountOpenAcross(ctx, []string{"donchian_breakout_v2", "donchian_breakout_v2_trail"})
	if err != nil {
		t.Fatalf("CountOpenAcross: %v", err)
	}
	if got.Positions != 4 {
		t.Errorf("Positions = %d, want 4(CLOSED は監視から降りるので数えない)", got.Positions)
	}
	if got.Symbols != 2 {
		t.Errorf("Symbols = %d, want 2(7203 / 6758。同じ銘柄の複数アームは 1 銘柄)", got.Symbols)
	}
	// 入口 donchian は 7203(兄弟 2 本)と 6758 の **2 銘柄**。3 建玉だが枠は銘柄で数える。
	if got.EntryArmSymbols != 2 {
		t.Errorf("EntryArmSymbols = %d, want 2 — FILTER 句が壊れると入口の枠が無効化される", got.EntryArmSymbols)
	}

	// 入口名を渡さなければ入口の集計は 0(枠を使わない呼び手)。`= ANY(空配列)` が
	// 全件マッチに化けないことの確認でもある。
	none, err := posRepo.CountOpenAcross(ctx, nil)
	if err != nil {
		t.Fatalf("CountOpenAcross(nil): %v", err)
	}
	if none.EntryArmSymbols != 0 {
		t.Errorf("EntryArmSymbols = %d, want 0(空の入口名で全件にマッチしてはいけない)", none.EntryArmSymbols)
	}
	// 既存の集計とずれていないこと(呼び手が 2 系統あるので片方だけ直すと静かにずれる)。
	n, err := posRepo.CountOpenAllSymbols(ctx)
	if err != nil {
		t.Fatalf("CountOpenAllSymbols: %v", err)
	}
	if none.Positions != n {
		t.Errorf("Positions = %d, CountOpenAllSymbols = %d — 2 つの集計がずれている", none.Positions, n)
	}
}
