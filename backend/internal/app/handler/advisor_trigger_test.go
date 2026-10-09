package handler_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"stockbot/backend/internal/app/handler"
)

// POST /api/advisor-trigger — dashboard の「今すぐ判断」ボタン。
// advisor OFF(注入なし)は 503、実行中は 409、受付は 202(非同期 — 結果は
// advisor_runs ジャーナルに出る)。
func TestPostAdvisorTrigger(t *testing.T) {
	t.Run("advisor OFF は 503", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil)
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/advisor-trigger", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("受付は 202", func(t *testing.T) {
		called := 0
		h := handler.New(nil, nil, nil, nil, nil, nil).
			WithAdvisorTrigger(func() error { called++; return nil })
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/advisor-trigger", nil))
		if rec.Code != http.StatusAccepted || called != 1 {
			t.Fatalf("status = %d called = %d, want 202 / 1 (body=%s)", rec.Code, called, rec.Body.String())
		}
	})

	t.Run("実行中は 409", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil).
			WithAdvisorTrigger(func() error { return errors.New("advise round already running") })
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/advisor-trigger", nil))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("非ループバック bind + token 無しは fail-close(既存の authorizeMutation)", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil).
			WithAuth("", true).
			WithAdvisorTrigger(func() error { return nil })
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/advisor-trigger", nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403(無認証のリモート操作を許してはいけない)", rec.Code)
		}
	})
}
