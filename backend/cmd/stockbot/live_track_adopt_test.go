package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 🚨 **ユニバースから外れた建玉を管理外にしない(live)。**
//
// live だけ `botCfg.Symbols` を直接回していたので、日次ユニバース(毎朝 07:00 に
// 書き換わる)から落ちた銘柄の**実弾建玉が丸ごと管理外**になっていた。
// 管理外になると OnTick も ReconcileTick も走らず、
// 台帳の TP / ratchet / max_hold が誰にも評価されず、broker 側の逆指値が約定しても
// 台帳に決済が書かれない(建玉が永久に OPEN のまま枠と日次損失の計算を狂わせる)。
//
// 🛑 **ソースを pin する**(harvest_track_test.go と同じ作法)。buildLiveTrack を
// 実際に呼ぶには立花セッションと DB が要り、テストから到達できない。合流を消す
// リファクタが**コンパイルも既存テストも通ってしまう**ので、ここで縛る。
func TestLiveTrackMergesHeldSymbolsIntoTheWatchSet(t *testing.T) {
	src := mustReadSource(t, "live_track.go")

	// 監視集合はユニバース **∪ 建玉のある銘柄**。live の台帳(st.positions)を渡すこと
	// (research の建玉を live に合流したら別トラックの建玉を実弾側で管理することになる)。
	if !strings.Contains(src, "watchedSymbols(ctx, st.positions, botCfg.Symbols)") {
		t.Error("live が建玉のある銘柄を監視対象に合流していない — ユニバースから落ちた" +
			"実弾建玉が OnTick も reconcile も受けなくなる")
	}
	// 🛑 合流ぶんは **arm できない**。holders に載せると selector がユニバース外の
	// 銘柄に新規の実弾建玉を出せてしまう(合流の目的は決済だけ)。
	if !strings.Contains(src, "if !adopted[sym] {") {
		t.Error("合流ぶんを holders から外していない — ユニバース外の銘柄で新規に建てられる")
	}
	// 合流ぶんに active config を当てない(no_trade 固定)。
	if !strings.Contains(src, "&& !adopted[sym]") {
		t.Error("合流ぶんに active config を当てている — no_trade 固定でないと新規建ての経路が開く")
	}
}

// watchedSymbols 自体の振る舞い(live が依存する側)。
func TestWatchedSymbolsAdoptsHeldSymbolsOutsideTheUniverse(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	now := time.Date(2026, 8, 24, 9, 0, 0, 0, clock.JST)
	if _, err := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "b1", Symbol: "4704", Side: order.SideBuy, Quantity: 100,
		OpenedAt: now, HoldingMode: order.HoldingMultiday,
	}); err != nil {
		t.Fatal(err)
	}
	// 翌朝のユニバースから 4704 が落ちた状況。
	all, adopted, err := watchedSymbols(ctx, repo, []string{"7203", "6501"})
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 1 || adopted[0] != "4704" {
		t.Fatalf("adopted = %v, want [4704]", adopted)
	}
	found := false
	for _, s := range all {
		if s == "4704" {
			found = true
		}
	}
	if !found {
		t.Fatal("建玉のある 4704 が監視対象に入っていない — OnTick も reconcile も走らない")
	}
}
