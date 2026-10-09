package command

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
)

func TestExtendMaxHold(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 18, 1, 0, 0, 0, time.UTC)
	repo := repository.NewInMemoryPositionRepo()
	id, _ := repo.Insert(ctx, port.PositionInsertInput{BrokerPositionID: "p1", Symbol: "7203", Side: order.SideBuy, Quantity: 100, OpenedAt: now})
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "emergency.flag"), nil)
	cmd := NewExtendMaxHold(repo, es)

	t.Run("valid extend adds minutes", func(t *testing.T) {
		res, err := cmd.Execute(ctx, id, 30)
		if err != nil {
			t.Fatal(err)
		}
		if res == nil || res.NewMaxMinutes != 30 {
			t.Fatalf("got %+v, want NewMaxMinutes=30", res)
		}
	})

	t.Run("out-of-range minutes rejected before touching repo", func(t *testing.T) {
		for _, m := range []int{0, -5, 721} {
			if _, err := cmd.Execute(ctx, id, m); !errors.Is(err, ErrInvalidExtendMinutes) {
				t.Errorf("add=%d: want ErrInvalidExtendMinutes, got %v", m, err)
			}
		}
	})

	t.Run("unknown id returns (nil,nil) for a 404 mapping", func(t *testing.T) {
		res, err := cmd.Execute(ctx, 99999, 30)
		if err != nil || res != nil {
			t.Fatalf("got (%+v, %v), want (nil, nil)", res, err)
		}
	})

	// repo の CAS は WHERE status='OPEN'。非 OPEN の id に延長が成功して 200 を返す
	// のは、閉じた建玉の max_hold が評価されない以上 operator への嘘になる。
	t.Run("closed position returns (nil,nil) — OPEN-only CAS", func(t *testing.T) {
		id2, _ := repo.Insert(ctx, port.PositionInsertInput{BrokerPositionID: "p2", Symbol: "7203", Side: order.SideBuy, Quantity: 100, OpenedAt: now})
		if err := repo.MarkClosed(ctx, id2, now); err != nil {
			t.Fatal(err)
		}
		res, err := cmd.Execute(ctx, id2, 30)
		if err != nil || res != nil {
			t.Fatalf("closed position: got (%+v, %v), want (nil, nil)", res, err)
		}
	})

	t.Run("emergency active rejects the manual override", func(t *testing.T) {
		if err := es.Trip("test", now); err != nil {
			t.Fatal(err)
		}
		if _, err := cmd.Execute(ctx, id, 30); !errors.Is(err, ErrEmergencyActive) {
			t.Fatalf("want ErrEmergencyActive during emergency, got %v", err)
		}
	})
}

// 🛑 多日保有の延長を「720分(12時間)」で縛ると、1日延ばすのに 2 回・10日で 20 回
// 叩くことになり、運用として成立しない(live の 4751/4901 で実際に
// 詰まった)。上限は**保有区分ごと**に決める — intraday は引けまでしか意味が無い
// ので 720 のまま、multiday は暦日で持つので 30 日。
func TestExtendMaxHold_CapDependsOnHoldingMode(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 1, 0, 0, 0, time.UTC)
	repo := repository.NewInMemoryPositionRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "emergency.flag"), nil)
	cmd := NewExtendMaxHold(repo, es)

	multi, _ := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "m1", Symbol: "4751", Side: order.SideBuy, Quantity: 100,
		OpenedAt: now, HoldingMode: order.HoldingMultiday, MaxHoldMinutes: 14400,
	})
	intra, _ := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "i1", Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		OpenedAt: now, HoldingMode: order.HoldingIntraday, MaxHoldMinutes: 60,
	})

	t.Run("multiday accepts days-scale minutes", func(t *testing.T) {
		res, err := cmd.Execute(ctx, multi, 30*24*60)
		if err != nil {
			t.Fatalf("multiday 30日の延長が弾かれた: %v", err)
		}
		if res == nil || res.NewMaxMinutes != 14400+30*24*60 {
			t.Fatalf("got %+v, want NewMaxMinutes=%d", res, 14400+30*24*60)
		}
	})

	t.Run("multiday still bounded", func(t *testing.T) {
		if _, err := cmd.Execute(ctx, multi, 30*24*60+1); !errors.Is(err, ErrInvalidExtendMinutes) {
			t.Fatalf("上限超えが通った: %v", err)
		}
	})

	// intraday の上限は据え置き。桁を間違えた延長(1日 = 1440分)を弾くのが目的。
	t.Run("intraday keeps the 720 cap", func(t *testing.T) {
		if _, err := cmd.Execute(ctx, intra, 1440); !errors.Is(err, ErrInvalidExtendMinutes) {
			t.Fatalf("intraday に 1440 分が通った: %v", err)
		}
		if _, err := cmd.Execute(ctx, intra, 720); err != nil {
			t.Fatalf("intraday 720 が弾かれた: %v", err)
		}
	})

	// 未知 id は「上限が決められない」= 400 ではなく 404 に落とす(既存契約)。
	t.Run("unknown id is still (nil,nil)", func(t *testing.T) {
		res, err := cmd.Execute(ctx, 987654, 60)
		if err != nil || res != nil {
			t.Fatalf("got (%+v, %v), want (nil, nil)", res, err)
		}
	})
}

// 🚨 多日保有の延長は「broker 側の守りは動いていない」を必ず応答に載せる。
// 載せないと「30 日持てるようにした = その間 SL がある」と読まれる。
func TestExtendMaxHold_MultidayCarriesTheGuardWarning(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 1, 0, 0, 0, time.UTC)
	repo := repository.NewInMemoryPositionRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "emergency.flag"), nil)
	cmd := NewExtendMaxHold(repo, es)
	multi, _ := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "m9", Symbol: "4751", Side: order.SideBuy, Quantity: 100,
		OpenedAt: now, HoldingMode: order.HoldingMultiday, MaxHoldMinutes: 14400,
	})
	res, err := cmd.Execute(ctx, multi, 43200)
	if err != nil {
		t.Fatal(err)
	}
	if res.Warning == "" {
		t.Fatal("多日保有の延長に守りの警告が付いていない")
	}
	intra, _ := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "i9", Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		OpenedAt: now, HoldingMode: order.HoldingIntraday, MaxHoldMinutes: 60,
	})
	res2, err := cmd.Execute(ctx, intra, 60)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Warning != "" {
		t.Fatalf("intraday に不要な警告が付いた: %q", res2.Warning)
	}
}
