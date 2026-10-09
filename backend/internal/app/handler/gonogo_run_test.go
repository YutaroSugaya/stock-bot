package handler_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"stockbot/backend/internal/app/handler"
	"stockbot/backend/internal/port"
)

// POST /api/gonogo/run — 画面の「未判定を判定」ボタン。arm 済みで判定が無い銘柄の go/no-go を
// 別プロセスで起動して即 return する(表示と記録だけ・発注経路は読まない)。
func TestPostGoNoGoRun(t *testing.T) {
	post := func(h *handler.Handler) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/gonogo/run", nil))
		return rec
	}
	// live タブ(api() が /api/live を前置する)からも同じ処理に届く。live track が無くても 503 にしない
	// (判定は research と live で共有のファイル)。
	t.Run("live タブのパスも同じ処理", func(t *testing.T) {
		called := 0
		h := handler.New(nil, nil, nil, nil, nil, nil).
			WithGoNoGoRun(func() ([]string, error) { called++; return []string{"6594"}, nil })
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/live/gonogo/run", nil))
		if rec.Code != http.StatusAccepted || called != 1 {
			t.Fatalf("status = %d called = %d", rec.Code, called)
		}
	})
	type body struct {
		Started bool     `json:"started"`
		Symbols []string `json:"symbols"`
	}
	t.Run("未配線は 503", func(t *testing.T) {
		if rec := post(handler.New(nil, nil, nil, nil, nil, nil)); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("受付は 202 で判定する銘柄を返す", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil).
			WithGoNoGoRun(func() ([]string, error) { return []string{"6594", "7203"}, nil })
		rec := post(h)
		var b body
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		if rec.Code != http.StatusAccepted || !b.Started || len(b.Symbols) != 2 {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("判定する銘柄が無ければ 200 で started=false", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil).
			WithGoNoGoRun(func() ([]string, error) { return nil, nil })
		rec := post(h)
		var b body
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || rec.Code != http.StatusOK || b.Started || b.Symbols == nil {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("実行中は 409", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil).
			WithGoNoGoRun(func() ([]string, error) { return nil, port.ErrGoNoGoRunning })
		if rec := post(h); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("起動できなければ 500", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil).
			WithGoNoGoRun(func() ([]string, error) { return nil, errors.New("no binary") })
		if rec := post(h); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("非ループバック bind + token 無しは fail-close", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil).WithAuth("", true).
			WithGoNoGoRun(func() ([]string, error) { return []string{"6594"}, nil })
		if rec := post(h); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}
