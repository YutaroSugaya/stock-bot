package repository

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/port"
)

func rec(id, sym string, at time.Time, st port.AdvisorRunStatus) port.AdvisorRunRecord {
	return port.AdvisorRunRecord{RunID: id, Symbol: sym, Status: st, StartedAt: at}
}

func TestInMemoryAdvisorRunRepo_InsertAndListNewestFirst(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryAdvisorRunRepo()
	t0 := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	_ = r.Insert(ctx, rec("a", "7203", t0, port.AdvisorRunSuccess))
	_ = r.Insert(ctx, rec("b", "7203", t0.Add(time.Hour), port.AdvisorRunCLIError))
	_ = r.Insert(ctx, rec("c", "6758", t0.Add(2*time.Hour), port.AdvisorRunSuccess))

	all, err := r.List(ctx, "", 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("want 3 rows, got %d err=%v", len(all), err)
	}
	if all[0].RunID != "c" || all[2].RunID != "a" {
		t.Fatalf("expected newest-first, got %s..%s", all[0].RunID, all[2].RunID)
	}

	bySym, _ := r.List(ctx, "7203", 0)
	if len(bySym) != 2 {
		t.Fatalf("symbol filter failed: %d", len(bySym))
	}

	limited, _ := r.List(ctx, "", 1)
	if len(limited) != 1 || limited[0].RunID != "c" {
		t.Fatalf("limit failed: %+v", limited)
	}
}

// run_id is the PK: re-inserting the same id replaces (matches the pg upsert).
func TestInMemoryAdvisorRunRepo_SameRunIDReplaces(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryAdvisorRunRepo()
	t0 := time.Now()
	_ = r.Insert(ctx, rec("a", "7203", t0, port.AdvisorRunCLIError))
	updated := rec("a", "7203", t0, port.AdvisorRunSuccess)
	updated.RegimeReason = "パニック確認"
	_ = r.Insert(ctx, updated)

	all, _ := r.List(ctx, "", 0)
	if len(all) != 1 {
		t.Fatalf("want 1 row after replace, got %d", len(all))
	}
	if all[0].Status != port.AdvisorRunSuccess || all[0].RegimeReason != "パニック確認" {
		t.Fatalf("row not replaced: %+v", all[0])
	}
}
