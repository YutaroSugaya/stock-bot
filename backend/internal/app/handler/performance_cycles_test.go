package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"stockbot/backend/internal/app/handler"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/query"
)

func jst(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, clock.JST) }

// 戦績はサイクル(期間)で切り替える。
// paper はサイクルごとに DB が違うので、サイクルは「どの台帳を・どの期間で」の組で持つ。
func TestPerformanceCyclesListAndSelect(t *testing.T) {
	archive := &stubTradeRepo{trades: []port.TradeRecord{
		{Symbol: "7203", Side: "BUY", Quantity: 100, ProfitLossJPY: 1000, CloseReason: "take_profit", ClosedAt: jst(2026, 8, 5).Add(10 * time.Hour)},
		{Symbol: "6758", Side: "BUY", Quantity: 100, ProfitLossJPY: -300, CloseReason: "stop_loss", ClosedAt: jst(2026, 8, 12).Add(10 * time.Hour)},
	}}
	current := &stubTradeRepo{}
	h := handler.New(nil, nil, nil, nil, nil, nil).
		WithPerformance(query.NewBuildForwardReport(current)).
		WithPerformanceCycles("c4", []handler.PerformanceCycle{
			{Key: "c1", Label: "期間1", Since: jst(2026, 7, 28), Until: jst(2026, 8, 8), Report: query.NewBuildForwardReport(archive)},
			{Key: "c4", Label: "期間4", Since: jst(2026, 9, 14)},
		})

	rec := getPerf(t, h, "/api/performance/cycles")
	if rec.Code != http.StatusOK {
		t.Fatalf("cycles status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var list struct {
		Default string `json:"default"`
		Cycles  []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"cycles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Default != "c4" || len(list.Cycles) != 2 || list.Cycles[0].Key != "c1" || list.Cycles[0].Label != "期間1" {
		t.Fatalf("cycles = %+v", list)
	}

	rec = getPerf(t, h, "/api/performance?cycle=c1")
	var got query.ForwardReportView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("cycle=c1 status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got.N != 1 || got.Since != "2026-07-28" || got.Until != "2026-08-08" {
		t.Fatalf("期間1 の台帳と期間で集計していない: N=%d since=%q until=%q", got.N, got.Since, got.Until)
	}

	// Report nil = そのトラックの既定の台帳を日付で切る。
	if rec = getPerf(t, h, "/api/performance?cycle=c4"); rec.Code != http.StatusOK {
		t.Fatalf("cycle=c4 status=%d", rec.Code)
	}
	if !current.gotSince.Equal(jst(2026, 9, 14)) {
		t.Fatalf("既定の台帳に since が渡っていない: %v", current.gotSince)
	}

	// 知らないサイクルは黙って全期間にせず 400。
	if rec = getPerf(t, h, "/api/performance?cycle=c9"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown cycle status = %d, want 400", rec.Code)
	}
}

// live は同じ live DB を日付で切る(サイクルの台帳は持たない)。
func TestLivePerformanceCyclesCutTheLiveLedgerByDate(t *testing.T) {
	live := &stubTradeRepo{trades: []port.TradeRecord{
		{Symbol: "4901", Side: "BUY", Quantity: 100, ProfitLossJPY: 500, CloseReason: "take_profit", ClosedAt: jst(2026, 9, 2).Add(10 * time.Hour)},
		{Symbol: "7220", Side: "BUY", Quantity: 100, ProfitLossJPY: -27200, CloseReason: "max_hold", ClosedAt: jst(2026, 9, 14).Add(9 * time.Hour)},
	}}
	h := handler.New(nil, nil, nil, nil, nil, nil).WithLiveTrack(&handler.LiveViews{
		Performance:  query.NewBuildForwardReport(live),
		CycleDefault: "c4",
		Cycles: []handler.PerformanceCycle{
			{Key: "c3", Label: "期間3", Since: jst(2026, 8, 24), Until: jst(2026, 9, 14)},
			{Key: "c4", Label: "期間4", Since: jst(2026, 9, 14)},
		},
	})
	if rec := getPerf(t, h, "/api/live/performance/cycles"); rec.Code != http.StatusOK {
		t.Fatalf("live cycles status = %d", rec.Code)
	}
	rec := getPerf(t, h, "/api/live/performance?cycle=c3")
	var got query.ForwardReportView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got.N != 1 || got.NetTotalJPY != 500 {
		t.Fatalf("期間3 の期間で切れていない: N=%d net=%v", got.N, got.NetTotalJPY)
	}
}
