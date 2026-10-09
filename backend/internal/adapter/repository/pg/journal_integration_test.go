//go:build integration

package pg

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 日次総評(段1)の SQL。**列名と日境界を機械で確かめる唯一の経路**が integration
// なので、ここが薄いと「静かに 0 件」で通ってしまう。
func TestPg_JournalRepo(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()
	// screen_snapshots は共通の全消しヘルパの対象外(監査/検定専用テーブル)なので
	// 自分で消す。行を消すだけ — スキーマは触らない(正本は migrations/)。
	if _, err := pool.Exec(ctx, `DELETE FROM screen_snapshots`); err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 8, 14, 0, 0, 0, 0, clock.JST)
	next := day.AddDate(0, 0, 1)
	inDay := time.Date(2026, 8, 14, 10, 0, 0, 0, clock.JST)
	prevDay := time.Date(2026, 8, 13, 10, 0, 0, 0, clock.JST)

	cfgRepo := NewConfigRepo(pool)
	if err := cfgRepo.EnsureExists(ctx, port.StrategyConfigRecord{
		ConfigID: "cfg-j", Symbol: "7203", Mode: "paper_config", StrategyName: "atr_breakout_v2", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	posRepo := NewPositionRepo(pool)
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "bp-j", Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 1000,
		StrategyConfigID: "cfg-j", HoldingMode: order.HoldingMultiday, ExecKind: order.ExecMarginSystem, OpenedAt: inDay,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// 前日に建てて当日また開いている建玉(as-of の対象)。
	if _, err := posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "bp-j2", Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 900,
		StrategyConfigID: "cfg-j", HoldingMode: order.HoldingMultiday, ExecKind: order.ExecMarginSystem, OpenedAt: prevDay,
	}); err != nil {
		t.Fatalf("insert prev: %v", err)
	}
	if ok, err := posRepo.ClaimForClose(ctx, id, inDay); err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	if ok, err := NewCloser(pool).CloseAndRecord(ctx, id, inDay, port.TradeRecord{
		PositionID: id, Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 1000, ClosePrice: 1030,
		ProfitLossJPY: 3000, FeeJPY: 100, CarryJPY: -20, CloseReason: "take_profit", ClosedAt: inDay,
	}); err != nil || !ok {
		t.Fatalf("close: %v", err)
	}
	// 同じ銘柄が複数ラウンドに現れる(実運用そのもの)。上位銘柄は重複排除されること。
	snaps := NewScreenSnapshotRepo(pool)
	for _, round := range []time.Time{inDay, inDay.Add(time.Hour)} {
		if err := snaps.InsertRound(ctx, []port.ScreenSnapshot{
			{RoundAt: round, Symbol: "7203", Strategy: "atr_breakout_v2", Triggered: true, Score: 1.5, Picked: true},
			{RoundAt: round, Symbol: "6501", Strategy: "atr_breakout_v2", Triggered: true, Score: 1.2},
			{RoundAt: round, Symbol: "9984", Strategy: "atr_breakout_v2", Triggered: false, Score: 0.1},
		}); err != nil {
			t.Fatalf("snapshots: %v", err)
		}
	}

	r := NewJournalRepo(pool)
	trades, err := r.JournalClosedTrades(ctx, day, next)
	if err != nil || len(trades) != 1 {
		t.Fatalf("trades = %+v (%v)", trades, err)
	}
	if trades[0].Strategy != "atr_breakout_v2" || trades[0].NetJPY() != 2880 {
		t.Fatalf("trade = %+v(net = gross − fee + carry)", trades[0])
	}
	opened, err := r.JournalOpenedPositions(ctx, day, next)
	if err != nil || len(opened) != 1 || opened[0].EntryPrice != 1000 {
		t.Fatalf("opened = %+v (%v) — 当日に建てた 1 本だけ", opened, err)
	}
	openAt, err := r.JournalOpenPositionsAsOf(ctx, next)
	if err != nil || len(openAt) != 1 || openAt[0].EntryPrice != 900 {
		t.Fatalf("as-of = %+v (%v) — 引け時点で開いているのは前日建ての 1 本", openAt, err)
	}
	scr, err := r.JournalScreens(ctx, day, next)
	if err != nil || len(scr) != 1 {
		t.Fatalf("screens = %+v (%v)", scr, err)
	}
	if scr[0].Triggered != 2 || scr[0].Picked != 1 {
		t.Fatalf("screens = %+v — ラウンド数ではなく銘柄数で数える", scr[0])
	}
	if len(scr[0].TopSymbols) != 2 || scr[0].TopSymbols[0] != "7203" {
		t.Fatalf("top = %+v — 銘柄で重複排除し score 降順", scr[0].TopSymbols)
	}
}
