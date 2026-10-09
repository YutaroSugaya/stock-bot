package repository_test

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/port"
)

type stubTrades struct {
	recs     []port.TradeRecord
	gotSince time.Time
}

func (s *stubTrades) ListClosedSince(_ context.Context, since time.Time) ([]port.TradeRecord, error) {
	s.gotSince = since
	return append([]port.TradeRecord(nil), s.recs...), nil
}

type stubStrategies map[int64]string

func (s stubStrategies) StrategyByPositionID(_ context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	for _, id := range ids {
		if n, ok := s[id]; ok {
			out[id] = n
		}
	}
	return out, nil
}

// 「全期間」の戦績は DB をまたいで合算する(期間ごとに DB が違う)。
// **建玉 id は DB ごとに 1 から振られて衝突する**ので、そのまま混ぜると
// 戦略名の解決が別の DB の建玉を指す。合算後の id は DB ごとに名前空間を分けること。
func TestMergedTradesKeepsPositionIDsApartAcrossDatabases(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)
	a := &stubTrades{recs: []port.TradeRecord{{PositionID: 1, Symbol: "7203", ClosedAt: t0.Add(2 * time.Hour)}}}
	b := &stubTrades{recs: []port.TradeRecord{{PositionID: 1, Symbol: "6758", ClosedAt: t0.Add(time.Hour)}}}
	m := repository.NewMergedTrades(
		repository.TradeSource{Trades: a, Strategies: stubStrategies{1: "bnf_reversion"}},
		repository.TradeSource{Trades: b, Strategies: stubStrategies{1: "high_volume_premium"}},
	)
	since := t0.Add(-time.Hour)
	recs, err := m.ListClosedSince(context.Background(), since)
	if err != nil || len(recs) != 2 {
		t.Fatalf("ListClosedSince: err=%v n=%d, want 2", err, len(recs))
	}
	if !a.gotSince.Equal(since) || !b.gotSince.Equal(since) {
		t.Fatal("since が全ソースに渡っていない")
	}
	if recs[0].Symbol != "6758" || recs[1].Symbol != "7203" {
		t.Fatalf("closed_at 昇順になっていない: %s, %s", recs[0].Symbol, recs[1].Symbol)
	}
	if recs[0].PositionID == recs[1].PositionID {
		t.Fatalf("別 DB の建玉 id が衝突したまま: %d", recs[0].PositionID)
	}
	names, err := m.StrategyByPositionID(context.Background(), []int64{recs[0].PositionID, recs[1].PositionID})
	if err != nil {
		t.Fatal(err)
	}
	if names[recs[0].PositionID] != "high_volume_premium" || names[recs[1].PositionID] != "bnf_reversion" {
		t.Fatalf("戦略名が別の DB の建玉を指している: %v", names)
	}
}

// 戦略を解決できないソース(resolver nil)は名前を返さない(捏造しない)。
func TestMergedTradesSkipsSourcesWithoutResolver(t *testing.T) {
	a := &stubTrades{recs: []port.TradeRecord{{PositionID: 5, ClosedAt: time.Now()}}}
	m := repository.NewMergedTrades(repository.TradeSource{Trades: a})
	recs, _ := m.ListClosedSince(context.Background(), time.Time{})
	names, err := m.StrategyByPositionID(context.Background(), []int64{recs[0].PositionID})
	if err != nil || len(names) != 0 {
		t.Fatalf("names=%v err=%v, want 空", names, err)
	}
}
