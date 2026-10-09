package query_test

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/query"
)

// MOCK rationale (TESTING.md 3用途): §1 system boundary (repository)。
type stubResolver struct {
	names  map[int64]string
	gotIDs []int64
}

func (s *stubResolver) StrategyByPositionID(_ context.Context, ids []int64) (map[int64]string, error) {
	s.gotIDs = append(s.gotIDs, ids...)
	out := map[int64]string{}
	for _, id := range ids {
		if n, ok := s.names[id]; ok {
			out[id] = n
		}
	}
	return out, nil
}

// config_id(`20260728-084554-4063`)は生成時刻と銘柄でしかなく「どの戦略で建てた
// のか」が読めない — 多戦略 audition では画面上の最重要情報。
func TestGetBotStatus_ResolvesStrategyName(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	id, err := repo.Insert(ctx, port.PositionInsertInput{
		Symbol: "4063", Side: order.SideBuy, Quantity: 100, EntryPrice: 3000,
		StrategyConfigID: "20260728-084554-4063", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	res := &stubResolver{names: map[int64]string{id: "abs_momentum"}}

	v, err := query.NewGetBotStatus(repo, nil, "paper_config", "paper").
		WithStrategyResolver(res).
		Execute(ctx, map[string]string{"4063": "cfg"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ps := v.PerSymbol["4063"].OpenPositions
	if len(ps) != 1 {
		t.Fatalf("建玉 = %d, want 1", len(ps))
	}
	if ps[0].Strategy != "abs_momentum" {
		t.Fatalf("Strategy = %q, want abs_momentum", ps[0].Strategy)
	}
	if ps[0].ConfigID != "20260728-084554-4063" {
		t.Errorf("ConfigID が消えている: %q", ps[0].ConfigID)
	}
}

// 「unknown」と埋めて実在する戦略名のように見せない(UI は config_id へ落ちる)。
func TestGetBotStatus_NoResolverLeavesStrategyEmpty(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	if _, err := repo.Insert(ctx, port.PositionInsertInput{
		Symbol: "4063", Side: order.SideBuy, Quantity: 100, EntryPrice: 3000, OpenedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	v, err := query.NewGetBotStatus(repo, nil, "paper_config", "paper").
		Execute(ctx, map[string]string{"4063": "cfg"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := v.PerSymbol["4063"].OpenPositions[0].Strategy; got != "" {
		t.Fatalf("Strategy = %q, want 空", got)
	}
}

// 隠すより見える方が監査になる(forward-report と同じ規約)。
func TestGetBotStatus_UnknownStrategyIsLabelled(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	if _, err := repo.Insert(ctx, port.PositionInsertInput{
		Symbol: "4063", Side: order.SideBuy, Quantity: 100, EntryPrice: 3000, OpenedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	v, err := query.NewGetBotStatus(repo, nil, "paper_config", "paper").
		WithStrategyResolver(&stubResolver{names: map[int64]string{}}).
		Execute(ctx, map[string]string{"4063": "cfg"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := v.PerSymbol["4063"].OpenPositions[0].Strategy; got != "unknown" {
		t.Fatalf("Strategy = %q, want unknown", got)
	}
}
