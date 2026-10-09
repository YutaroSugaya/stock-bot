package query_test

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/query"
)

func TestListOpenPositions_Execute(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 18, 1, 0, 0, 0, time.UTC)
	repo := repository.NewInMemoryPositionRepo()
	mustInsert(t, repo, port.PositionInsertInput{BrokerPositionID: "a", Symbol: "7203", Side: order.SideBuy, Quantity: 100, OpenedAt: now})
	mustInsert(t, repo, port.PositionInsertInput{BrokerPositionID: "b", Symbol: "7203", Side: order.SideSell, Quantity: 200, OpenedAt: now})
	mustInsert(t, repo, port.PositionInsertInput{BrokerPositionID: "c", Symbol: "6758", Side: order.SideBuy, Quantity: 300, OpenedAt: now})

	q := query.NewListOpenPositions(repo)

	t.Run("returns only the requested symbol as DTOs", func(t *testing.T) {
		views, err := q.Execute(ctx, "7203")
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 2 {
			t.Fatalf("got %d views, want 2", len(views))
		}
		for _, v := range views {
			if v.Symbol != "7203" {
				t.Errorf("unexpected symbol %q", v.Symbol)
			}
			if v.Status != string(position_StatusOpen) {
				t.Errorf("status = %q, want OPEN", v.Status)
			}
			if v.Side != "BUY" && v.Side != "SELL" {
				t.Errorf("unexpected side %q", v.Side)
			}
		}
	})

	t.Run("unknown symbol returns empty, non-nil slice", func(t *testing.T) {
		views, err := q.Execute(ctx, "9999")
		if err != nil {
			t.Fatal(err)
		}
		if views == nil || len(views) != 0 {
			t.Fatalf("got %+v, want empty non-nil slice", views)
		}
	})
}

const position_StatusOpen = "OPEN"

func mustInsert(t *testing.T, r *repository.InMemoryPositionRepo, in port.PositionInsertInput) {
	t.Helper()
	if _, err := r.Insert(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

// 円差だけだと、呼値 5 円の銘柄(3000〜5000円)で「+100」を tick 100 = TP 到達と
// 誤読する(実際は 20 tick)。
func TestOpenPositionViewCarriesTickGeometry(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	mustInsert(t, repo, port.PositionInsertInput{
		BrokerPositionID: "bp-2", Symbol: "6963", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 4625, TickSizeAtEntry: 5, TakeProfitJPY: 100, StopLossJPY: 50,
		OpenedAt: time.Date(2026, 7, 27, 1, 0, 0, 0, time.UTC),
	})
	got, err := query.NewListOpenPositions(repo).Execute(context.Background(), "6963")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	v := got[0]
	if v.TickSizeAtEntry != 5 || v.TakeProfitJPY != 100 || v.StopLossJPY != 50 {
		t.Fatalf("tick geometry が DTO に出ていない: %+v", v)
	}
}

// トレールアームの建玉は TP を**意図的に持たない**ので、DTO が ratchet を出さないと
// TP 列が '–' になり「出口が何か」が画面から消える。実際に 285A /
// 7735 / 6526 / 5803 が建玉つきで出口不明のまま表示されていた。
func TestOpenPositionViewCarriesRatchetGeometryForTrailArm(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	id, err := repo.Insert(context.Background(), port.PositionInsertInput{
		BrokerPositionID: "bp-3", Symbol: "285A", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 41050, TickSizeAtEntry: 10,
		TakeProfitJPY: 0, StopLossJPY: 3070.4, // TP なし = uncapped
		RatchetArmJPY: 8757.9, RatchetGivebackJPY: 21894.6,
		OpenedAt: time.Date(2026, 7, 30, 4, 3, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := repo.UpdateExcursion(context.Background(), id, 5780, 0, false); err != nil {
		t.Fatalf("UpdateExcursion: %v", err)
	}
	got, err := query.NewListOpenPositions(repo).Execute(context.Background(), "285A")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	v := got[0]
	if v.RatchetArmJPY != 8757.9 || v.RatchetGivebackJPY != 21894.6 {
		t.Errorf("ratchet 幅が DTO に出ていない: arm=%v giveback=%v", v.RatchetArmJPY, v.RatchetGivebackJPY)
	}
	if v.PeakUnrealizedJPY != 5780 {
		t.Errorf("PeakUnrealizedJPY = %v, want 5780(arm までの距離が計算できない)", v.PeakUnrealizedJPY)
	}
	if v.RatchetArmed {
		t.Errorf("RatchetArmed = true, want false(peak 5780 < arm 8757.9)")
	}
}

// 作動済みトレールは脚を2本持ち(giveback 線と SL)、**遠い方には価格が先に届かない**。
// 「実際に効く方」を出さないと画面は到達し得ない値段を決済ラインとして見せる
// (285A: giveback 線 30,045.4 は SL 37,980 の外側)。
func TestOpenPositionViewCarriesEffectiveProtectiveExit(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	id, err := repo.Insert(context.Background(), port.PositionInsertInput{
		BrokerPositionID: "bp-4", Symbol: "285A", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 41050, StopLossPrice: 37980,
		RatchetArmJPY: 8757.857142857143, RatchetGivebackJPY: 21894.64285714286,
		OpenedAt: time.Date(2026, 7, 30, 4, 3, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := repo.UpdateExcursion(context.Background(), id, 10890, 0, true); err != nil {
		t.Fatalf("UpdateExcursion: %v", err)
	}
	got, err := query.NewListOpenPositions(repo).Execute(context.Background(), "285A")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	v := got[0]
	if v.ProtectiveExitPrice != 37980 || v.ProtectiveExitReason != "stop_loss" {
		t.Fatalf("実際に効く守り = (%v, %q), want (37980, \"stop_loss\") — トレール線 30045.4 は SL の外側",
			v.ProtectiveExitPrice, v.ProtectiveExitReason)
	}
}

// 時間切れ(max_hold)は**いつ来るか画面に出ていない**と、朝の寄りで突然成行決済されて
// 初めて気付く(7220 / 6841)。期限は凍結値から domain が決めるので、
// 画面が opened_at + 分 を JS で計算し直さないよう View に載せる。
func TestOpenPositionViewCarriesMaxHoldDeadline(t *testing.T) {
	opened := time.Date(2026, 9, 4, 0, 2, 31, 0, time.UTC) // 09:02:31 JST
	repo := repository.NewInMemoryPositionRepo()
	mustInsert(t, repo, port.PositionInsertInput{
		BrokerPositionID: "bp-mh", Symbol: "6841", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 4727, MaxHoldMinutes: 10 * 1440, OpenedAt: opened,
	})
	mustInsert(t, repo, port.PositionInsertInput{
		BrokerPositionID: "bp-ext", Symbol: "6841", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 4727, MaxHoldMinutes: 60, ExtensionMaxMinutes: 30, ExtensionUnrealizedJPY: 5,
		OpenedAt: opened,
	})
	mustInsert(t, repo, port.PositionInsertInput{
		BrokerPositionID: "bp-none", Symbol: "6841", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 4727, OpenedAt: opened,
	})
	got, err := query.NewListOpenPositions(repo).Execute(context.Background(), "6841")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d views, want 3", len(got))
	}
	var plain, ext, none *query.OpenPositionView
	for i := range got {
		switch {
		case got[i].MaxHoldMinutes == 10*1440:
			plain = &got[i]
		case got[i].MaxHoldMinutes == 60:
			ext = &got[i]
		case got[i].MaxHoldMinutes == 0:
			none = &got[i]
		}
	}
	if plain == nil || ext == nil || none == nil {
		t.Fatalf("max_hold_minutes が View に載っていない: %+v", got)
	}
	if plain.MaxHoldUntil == nil || !plain.MaxHoldUntil.Equal(opened.Add(10*24*time.Hour)) {
		t.Fatalf("max_hold_until = %v, want %v(建玉時刻 + 10 暦日)", plain.MaxHoldUntil, opened.Add(10*24*time.Hour))
	}
	if plain.MaxHoldHardUntil != nil {
		t.Fatalf("延長なしの建玉に max_hold_hard_until = %v(nil であるべき)", plain.MaxHoldHardUntil)
	}
	if ext.MaxHoldHardUntil == nil || !ext.MaxHoldHardUntil.Equal(opened.Add(90*time.Minute)) {
		t.Fatalf("max_hold_hard_until = %v, want %v(期限 + 延長上限)", ext.MaxHoldHardUntil, opened.Add(90*time.Minute))
	}
	// 0 = 無期限。ゼロ時刻を出すと画面が「0001-01-01 に時間切れ」と描く。
	if none.MaxHoldUntil != nil || none.MaxHoldHardUntil != nil {
		t.Fatalf("無期限の建玉に期限が出ている: until=%v hard=%v", none.MaxHoldUntil, none.MaxHoldHardUntil)
	}
}

// 株式分割で言い直した建玉は画面で分かる(株数が 100 の倍数でなくなる・建値が格子外になる理由)。
func TestListOpenPositions_ShowsSplitFactor(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	mustInsert(t, repo, port.PositionInsertInput{BrokerPositionID: "a", Symbol: "6368", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 11570, OpenedAt: time.Now()})
	ps, _ := repo.ListOpenOrClosing(ctx, "6368")
	adj, err := position.SplitAdjusted(ps[0], 5, time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := repo.ApplySplit(ctx, adj); !ok {
		t.Fatal("ApplySplit")
	}
	views, _ := query.NewListOpenPositions(repo).Execute(ctx, "6368")
	if len(views) != 1 || views[0].SplitFactor != 5 || views[0].SplitAdjustedOn != "2026-09-29" {
		t.Fatalf("view = %+v", views)
	}
}
