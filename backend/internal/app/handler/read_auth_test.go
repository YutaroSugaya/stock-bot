package handler_test

import (
	"net/http"
	"testing"

	"stockbot/backend/internal/app/handler"
)

// loopback の外に bind したら、参照系(GET)も token で守る。
// 変更系だけ守って GET を素通しにすると、同じネットワークの誰でも建玉と損益を読める。
func TestReadRoutesRequireTokenOffLoopback(t *testing.T) {
	t.Run("非 loopback + token 無し: GET も 403(fail-close)", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil).WithAuth("", true).Routes()
		rec := serve(h, http.MethodGet, "/api/advisor-runs", "stockbot.lan:8090", nil)
		wantCode(t, rec, http.StatusForbidden, "exposed bind without token must not serve reads")
	})
	t.Run("非 loopback + token あり: 無しは 401・正しければ通る", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil).WithAuth("t0ken", true).Routes()
		rec := serve(h, http.MethodGet, "/api/advisor-runs", "stockbot.lan:8090", nil)
		wantCode(t, rec, http.StatusUnauthorized, "missing token on an exposed bind")
		rec = serve(h, http.MethodGet, "/api/advisor-runs", "stockbot.lan:8090",
			map[string]string{"X-Api-Token": "wrong"})
		wantCode(t, rec, http.StatusUnauthorized, "wrong token on an exposed bind")
		rec = serve(h, http.MethodGet, "/api/advisor-runs", "stockbot.lan:8090",
			map[string]string{"X-Api-Token": "t0ken"})
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Fatalf("correct token must pass the read guard: status %d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("loopback bind は従来どおり token 無しで読める", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil).WithAuth("", false).Routes()
		rec := serve(h, http.MethodGet, "/api/advisor-runs", "127.0.0.1:8090", nil)
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Fatalf("loopback reads must stay open: status %d", rec.Code)
		}
	})
}
