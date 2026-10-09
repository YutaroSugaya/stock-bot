package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/app/handler"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/command"
)

// fakeExtender stands in for the extend-MaxHold usecase.
// MOCK rationale (TESTING.md 3用途): §1 system boundary (usecase under the handler).
type fakeExtender struct {
	res *port.MaxHoldExtended
	err error
}

func (f fakeExtender) Execute(context.Context, int64, int) (*port.MaxHoldExtended, error) {
	return f.res, f.err
}

func post(t *testing.T, h *handler.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/positions/extend", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

func TestPostExtendMaxHold(t *testing.T) {
	t.Run("success returns 200 with new deadline", func(t *testing.T) {
		ext := fakeExtender{res: &port.MaxHoldExtended{PositionID: 1, NewMaxMinutes: 90}}
		h := handler.New(nil, nil, nil, ext, nil, nil)
		rec := post(t, h, `{"position_id":1,"add_minutes":30}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "90") {
			t.Errorf("body %q missing new minutes", rec.Body.String())
		}
	})

	// 🛑 上限の数字は handler が持たない(保有区分で変わるので usecase しか判定できない)。
	// handler が弾くのは「正の整数か」だけで、範囲外は usecase の
	// ErrInvalidExtendMinutes を 400 に写して返す。
	t.Run("non-positive add_minutes returns 400", func(t *testing.T) {
		h := handler.New(nil, nil, nil, fakeExtender{}, nil, nil)
		for _, body := range []string{`{"position_id":1,"add_minutes":0}`, `{"position_id":1,"add_minutes":-5}`} {
			if rec := post(t, h, body); rec.Code != http.StatusBadRequest {
				t.Errorf("body %s: status = %d, want 400", body, rec.Code)
			}
		}
	})

	t.Run("usecase range error maps to 400", func(t *testing.T) {
		h := handler.New(nil, nil, nil, fakeExtender{err: command.ErrInvalidExtendMinutes}, nil, nil)
		if rec := post(t, h, `{"position_id":1,"add_minutes":999999}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
		}
	})

	// 多日保有の 30 日延長が handler で落ちない(live 4751/4901)。
	t.Run("multiday-scale minutes reach the usecase", func(t *testing.T) {
		ext := fakeExtender{res: &port.MaxHoldExtended{PositionID: 1, NewMaxMinutes: 14400 + 30*24*60}}
		h := handler.New(nil, nil, nil, ext, nil, nil)
		if rec := post(t, h, `{"position_id":1,"add_minutes":43200}`); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown position returns 404", func(t *testing.T) {
		h := handler.New(nil, nil, nil, fakeExtender{res: nil}, nil, nil)
		if rec := post(t, h, `{"position_id":999,"add_minutes":30}`); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("nil extender returns 503", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil)
		if rec := post(t, h, `{"position_id":1,"add_minutes":30}`); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
	})
}

// postAuth posts to a mutating endpoint with an optional token header.
func postAuth(t *testing.T, h *handler.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(`{"position_id":1,"add_minutes":30}`))
	if token != "" {
		req.Header.Set("X-Api-Token", token)
	}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

func TestControlAuth(t *testing.T) {
	ext := fakeExtender{res: &port.MaxHoldExtended{PositionID: 1, NewMaxMinutes: 90}}

	t.Run("token set: correct token passes, wrong/absent rejected", func(t *testing.T) {
		h := handler.New(nil, nil, nil, ext, nil, nil).WithAuth("s3cret", false)
		if rec := postAuth(t, h, "/api/positions/extend", "s3cret"); rec.Code != http.StatusOK {
			t.Fatalf("correct token: status %d, want 200", rec.Code)
		}
		if rec := postAuth(t, h, "/api/positions/extend", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong token: status %d, want 401", rec.Code)
		}
		if rec := postAuth(t, h, "/api/positions/extend", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("no token: status %d, want 401", rec.Code)
		}
	})

	t.Run("non-loopback bind without token fails closed", func(t *testing.T) {
		h := handler.New(nil, nil, nil, ext, nil, nil).WithAuth("", true)
		if rec := postAuth(t, h, "/api/positions/extend", ""); rec.Code != http.StatusForbidden {
			t.Fatalf("exposed + no token: status %d, want 403", rec.Code)
		}
	})

	t.Run("emergency endpoints are guarded too", func(t *testing.T) {
		h := handler.New(nil, nil, stubEmergency{}, nil, nil, nil).WithAuth("s3cret", false)
		if rec := postAuth(t, h, "/api/emergency-stop", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("emergency-stop no token: status %d, want 401", rec.Code)
		}
		if rec := postAuth(t, h, "/api/emergency-resume", "s3cret"); rec.Code != http.StatusOK {
			t.Fatalf("emergency-resume correct token: status %d, want 200", rec.Code)
		}
	})

	t.Run("loopback dev without token still allowed", func(t *testing.T) {
		h := handler.New(nil, nil, nil, ext, nil, nil) // no WithAuth = loopback dev default
		if rec := postAuth(t, h, "/api/positions/extend", ""); rec.Code != http.StatusOK {
			t.Fatalf("loopback dev: status %d, want 200", rec.Code)
		}
	})
}

// stubEmergency is a no-op EmergencyController for the auth tests.
type stubEmergency struct{}

func (stubEmergency) Active() bool                 { return false }
func (stubEmergency) Reason() string               { return "" }
func (stubEmergency) Trip(string, time.Time) error { return nil }
func (stubEmergency) Resume() error                { return nil }
