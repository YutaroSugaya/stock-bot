package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"stockbot/backend/internal/app/handler"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/query"
)

type stubRunRepo struct {
	sym   string
	limit int
}

func (s *stubRunRepo) Insert(context.Context, port.AdvisorRunRecord) error { return nil }

func (s *stubRunRepo) List(_ context.Context, symbol string, limit int) ([]port.AdvisorRunRecord, error) {
	s.sym, s.limit = symbol, limit
	return []port.AdvisorRunRecord{{
		RunID: "r1", Symbol: "7203", Status: port.AdvisorRunSuccess,
		StartedAt: time.Now(), FinishedAt: time.Now().Add(time.Second),
		RegimeType: "panic_selloff", RegimeReason: "5日で-12%",
	}}, nil
}

func TestGetAdvisorRuns(t *testing.T) {
	repo := &stubRunRepo{}
	h := handler.New(nil, nil, nil, nil, nil, nil).WithAdvisorRuns(query.NewListAdvisorRuns(repo))

	t.Run("判断一覧を返す", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/advisor-runs?symbol=7203&limit=5", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var out []query.AdvisorRunView
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
		}
		if len(out) != 1 || out[0].RegimeReason != "5日で-12%" {
			t.Fatalf("views = %+v", out)
		}
		if repo.sym != "7203" || repo.limit != 5 {
			t.Fatalf("query args = %q/%d", repo.sym, repo.limit)
		}
	})

	t.Run("advisor 未接続でも 200 + 空配列(パネルが壊れない)", func(t *testing.T) {
		rec := httptest.NewRecorder()
		bare := handler.New(nil, nil, nil, nil, nil, nil)
		bare.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/advisor-runs", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Body.String(); got != "[]\n" && got != "[]" {
			t.Fatalf("body = %q, want empty array", got)
		}
	})

	t.Run("不正な limit は既定にフォールバック", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/advisor-runs?limit=abc", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if repo.limit != 20 {
			t.Fatalf("limit = %d, want default 20", repo.limit)
		}
	})
}
