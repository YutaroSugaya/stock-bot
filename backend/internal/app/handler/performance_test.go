package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"stockbot/backend/internal/app/handler"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/query"
)

// stubTradeRepo は closed 台帳の代役。
// MOCK rationale (TESTING.md 3用途): §1 system boundary (handler の下の repository)。
// performance ハンドラは ListClosedSince しか呼ばない。残りは埋め込みの nil のままで、
// 呼ばれたら panic する = 「read 経路が書き込み系に触っていない」ことを型で保つ。
type stubTradeRepo struct {
	port.TradeRepository
	trades   []port.TradeRecord
	gotSince time.Time
}

func (s *stubTradeRepo) ListClosedSince(_ context.Context, since time.Time) ([]port.TradeRecord, error) {
	s.gotSince = since
	return s.trades, nil
}

func getPerf(t *testing.T, h *handler.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

// 戦績パネルの読み出し面。read-only なので token 無しでも 200(status/dashboard と同格)。
func TestGetPerformance(t *testing.T) {
	repo := &stubTradeRepo{trades: []port.TradeRecord{
		{Symbol: "7203", Side: "BUY", Quantity: 100, ProfitLossJPY: 1000, FeeJPY: 200,
			CloseReason: "take_profit", ClosedAt: time.Date(2026, 7, 27, 10, 0, 0, 0, clock.JST)},
	}}
	h := handler.New(nil, nil, nil, nil, nil, nil).
		WithPerformance(query.NewBuildForwardReport(repo))

	rec := getPerf(t, h, "/api/performance")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got query.ForwardReportView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if got.N != 1 || got.NetTotalJPY != 800 {
		t.Fatalf("集計が届いていない: %+v", got)
	}
	if len(got.ByDay) != 1 || got.ByDay[0].Day != "2026-07-27" {
		t.Fatalf("日別が届いていない: %+v", got.ByDay)
	}
	if !repo.gotSince.IsZero() {
		t.Fatalf("since 未指定は全期間(zero time): %v", repo.gotSince)
	}
}

// since=YYYY-MM-DD は JST 0時として repo に降りる(日付境界を UTC で切らない)。
func TestGetPerformanceSince(t *testing.T) {
	repo := &stubTradeRepo{}
	h := handler.New(nil, nil, nil, nil, nil, nil).
		WithPerformance(query.NewBuildForwardReport(repo))

	if rec := getPerf(t, h, "/api/performance?since=2026-07-24"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	want := time.Date(2026, 7, 24, 0, 0, 0, 0, clock.JST)
	if !repo.gotSince.Equal(want) {
		t.Fatalf("since = %v, want %v (JST 0時)", repo.gotSince, want)
	}
}

// 壊れた since は黙って全期間にせず 400(別物を返さない)。
func TestGetPerformanceRejectsBadSince(t *testing.T) {
	h := handler.New(nil, nil, nil, nil, nil, nil).
		WithPerformance(query.NewBuildForwardReport(&stubTradeRepo{}))
	if rec := getPerf(t, h, "/api/performance?since=nonsense"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// 未配線(trade repo 無し構成)は 503 — 空の戦績を「損益ゼロ」と誤読させない。
func TestGetPerformanceNotWired(t *testing.T) {
	h := handler.New(nil, nil, nil, nil, nil, nil)
	if rec := getPerf(t, h, "/api/performance"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
