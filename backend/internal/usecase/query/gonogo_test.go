package query

import (
	"context"
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

type stubGoNoGo struct {
	date string
	recs map[string]port.GoNoGoRecord
	err  error
}

func (s *stubGoNoGo) ForDate(_ context.Context, date string) (map[string]port.GoNoGoRecord, error) {
	s.date = date
	return s.recs, s.err
}

func TestListGoNoGo_TodayAndMissingArmed(t *testing.T) {
	ok := port.GoNoGoRecord{Symbol: "6594", Status: port.GoNoGoStatusOK, JudgedAt: time.Date(2026, 10, 2, 8, 16, 0, 0, clock.JST)}
	ok.Verdict, ok.Category = port.GoNoGoNoGo, "accounting_fraud"
	ok.Sources = []port.GoNoGoSource{{URL: "https://a.example", Title: "t"}}
	failed := port.GoNoGoRecord{Symbol: "7203", Status: port.GoNoGoStatusError, Error: "network"}
	r := &stubGoNoGo{recs: map[string]port.GoNoGoRecord{"6594": ok, "7203": failed}}
	// 0:30 UTC = 9:30 JST(日付は JST で切る)。
	q := NewListGoNoGo(r, clock.Fixed(time.Date(2026, 10, 2, 0, 30, 0, 0, time.UTC)))
	v := q.Execute(context.Background(), []string{"6594", "7203", "9984"})
	if r.date != "2026-10-02" || v.Date != "2026-10-02" {
		t.Fatalf("今日(JST)の判定を読む: %q", r.date)
	}
	if v.Symbols["6594"].Verdict != port.GoNoGoNoGo || len(v.Symbols["6594"].Sources) != 1 || v.Symbols["6594"].JudgedAt == "" {
		t.Fatalf("6594: %+v", v.Symbols["6594"])
	}
	if v.Symbols["7203"].Status != port.GoNoGoStatusError {
		t.Fatalf("失敗も出す: %+v", v.Symbols["7203"])
	}
	// arm 済みなのに成功した判定が無い銘柄(失敗・未判定)。
	if len(v.MissingArmed) != 2 || v.MissingArmed[0] != "7203" || v.MissingArmed[1] != "9984" {
		t.Fatalf("missing = %v", v.MissingArmed)
	}
}

// 読めない・無いときは「未判定」と出すだけで落ちない。
func TestListGoNoGo_ReadErrorIsShownNotFatal(t *testing.T) {
	v := NewListGoNoGo(&stubGoNoGo{err: errors.New("perm")}, clock.Fixed(time.Now())).Execute(context.Background(), nil)
	if v.Error == "" || v.Symbols == nil {
		t.Fatalf("%+v", v)
	}
}
