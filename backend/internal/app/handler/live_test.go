package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 🛑 live track 未配線なら /api/live/* は **503**。空配列を返してはいけない —
// 「live は動いているが建玉が無い」と読めてしまい、止まっている実弾を動いていると
// 誤認する。
func TestLiveRoutes_503WhenNotWired(t *testing.T) {
	h := New(nil, nil, nil, nil, func() map[string]string { return nil }, nil)
	mux := h.Routes()
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/live/dashboard"},
		{"GET", "/api/live/status"},
		{"GET", "/api/live/positions?symbol=7203"},
		{"GET", "/api/live/performance"},
		{"POST", "/api/live/positions/close"},
		{"POST", "/api/live/positions/extend"},
		{"GET", "/api/live/positions/extend-options?position_id=1"},
		{"POST", "/api/live/emergency-stop"},
		{"POST", "/api/live/emergency-resume"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Origin", "http://127.0.0.1:8090")
		req.Host = "127.0.0.1:8090"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s → %d, want 503(live 無しは 503。空応答にしない)", c.method, c.path, w.Code)
		}
	}
}

// 🛑 **live の flatten-all は提供しない**。全清算を人間がボタン 1 つでやる操作に
// しない(HYBRID Step 5)。research 側の /api/flatten-all は従来どおり残す。
func TestLiveRoutes_NoFlattenAll(t *testing.T) {
	h := New(nil, nil, nil, nil, func() map[string]string { return nil }, nil)
	req := httptest.NewRequest("POST", "/api/live/flatten-all", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8090")
	req.Host = "127.0.0.1:8090"
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("/api/live/flatten-all → %d, want 404(live の全清算は提供しない)", w.Code)
	}
}

// 既存ルート(research)は live の有無に関係なく不変。
func TestResearchRoutesUnchangedByLive(t *testing.T) {
	h := New(nil, nil, nil, nil, func() map[string]string { return nil }, nil)
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("/healthz → %d, want 200", w.Code)
	}
}
