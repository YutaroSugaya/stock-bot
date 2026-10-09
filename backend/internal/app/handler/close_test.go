package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stockbot/backend/internal/app/handler"
	"stockbot/backend/internal/usecase/command"
)

type fakeManualCloser struct {
	res      command.CloseAllResult
	one      *command.CloseOneResult
	oneErr   error
	allCalls int
	oneIDs   []int64
}

func (f *fakeManualCloser) Execute(context.Context) (command.CloseAllResult, error) {
	f.allCalls++
	return f.res, nil
}

func (f *fakeManualCloser) CloseOne(_ context.Context, id int64) (*command.CloseOneResult, error) {
	f.oneIDs = append(f.oneIDs, id)
	return f.one, f.oneErr
}

func postTo(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// research / paper のサイクル境界の帳簿締め。応答は closed/failed/skipped_external。
func TestFlattenAll_ClosesAndReports(t *testing.T) {
	fake := &fakeManualCloser{res: command.CloseAllResult{Closed: 12, Failed: 1, SkippedExternal: 2}}
	h := handler.New(nil, nil, nil, nil, nil, nil).WithManualClose(fake, "research").Routes()

	rec := postTo(t, h, "/api/flatten-all", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var got map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if got["closed"] != 12 || got["failed"] != 1 || got["skipped_external"] != 2 {
		t.Fatalf("unexpected body: %v", got)
	}
}

// live の全清算は人間の判断。このエンドポイントは research/paper 専用。
func TestFlattenAll_RefusedInLiveConfig(t *testing.T) {
	fake := &fakeManualCloser{}
	h := handler.New(nil, nil, nil, nil, nil, nil).WithManualClose(fake, "live_config").Routes()

	rec := postTo(t, h, "/api/flatten-all", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (live で全清算を通してはいけない)", rec.Code)
	}
	if fake.allCalls != 0 {
		t.Fatalf("usecase must not run in live_config, calls=%d", fake.allCalls)
	}
}

func TestFlattenAll_UnwiredIsServiceUnavailable(t *testing.T) {
	h := handler.New(nil, nil, nil, nil, nil, nil).Routes()
	if rec := postTo(t, h, "/api/flatten-all", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestClosePosition(t *testing.T) {
	t.Run("成行決済が通る", func(t *testing.T) {
		fake := &fakeManualCloser{one: &command.CloseOneResult{PositionID: 7, Symbol: "7203", Quantity: 100, Closed: true}}
		h := handler.New(nil, nil, nil, nil, nil, nil).WithManualClose(fake, "research").Routes()
		rec := postTo(t, h, "/api/positions/close", `{"position_id":7}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if len(fake.oneIDs) != 1 || fake.oneIDs[0] != 7 {
			t.Fatalf("usecase got ids %v, want [7]", fake.oneIDs)
		}
	})

	t.Run("未知の id は 404", func(t *testing.T) {
		fake := &fakeManualCloser{oneErr: command.ErrPositionNotFound}
		h := handler.New(nil, nil, nil, nil, nil, nil).WithManualClose(fake, "research").Routes()
		if rec := postTo(t, h, "/api/positions/close", `{"position_id":99}`); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("OPEN でない建玉は 409", func(t *testing.T) {
		fake := &fakeManualCloser{oneErr: command.ErrPositionNotOpen}
		h := handler.New(nil, nil, nil, nil, nil, nil).WithManualClose(fake, "research").Routes()
		if rec := postTo(t, h, "/api/positions/close", `{"position_id":7}`); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})

	t.Run("external は 409", func(t *testing.T) {
		fake := &fakeManualCloser{oneErr: command.ErrPositionExternal}
		h := handler.New(nil, nil, nil, nil, nil, nil).WithManualClose(fake, "research").Routes()
		if rec := postTo(t, h, "/api/positions/close", `{"position_id":7}`); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})

	t.Run("position_id 不正は 400", func(t *testing.T) {
		fake := &fakeManualCloser{}
		h := handler.New(nil, nil, nil, nil, nil, nil).WithManualClose(fake, "research").Routes()
		for _, body := range []string{`{"position_id":0}`, `not json`} {
			if rec := postTo(t, h, "/api/positions/close", body); rec.Code != http.StatusBadRequest {
				t.Fatalf("body=%q status = %d, want 400", body, rec.Code)
			}
		}
		if len(fake.oneIDs) != 0 {
			t.Fatalf("不正な入力で usecase を呼んではいけない: %v", fake.oneIDs)
		}
	})
}

// 新しい mutating エンドポイントも CSRF ガード(authorizeMutation)の内側にある。
func TestManualCloseEndpointsRejectCrossSite(t *testing.T) {
	fake := &fakeManualCloser{one: &command.CloseOneResult{Closed: true}}
	for _, path := range []string{"/api/flatten-all", "/api/positions/close"} {
		h := handler.New(nil, nil, nil, nil, nil, nil).WithManualClose(fake, "research").Routes()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"position_id":1}`))
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", path, rec.Code)
		}
	}
	if fake.allCalls != 0 || len(fake.oneIDs) != 0 {
		t.Fatalf("cross-site で usecase が動いてしまっている")
	}
}
