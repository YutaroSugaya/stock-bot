package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeReplacer struct {
	skipped int
	called  int
	n       int
	errs    []string
}

func (f *fakeReplacer) Execute(context.Context) (int, int, []string) {
	f.called++
	return f.n, f.skipped, f.errs
}

// 🛑 **守りの置き直しは「叩いたときだけ」走る入口を持つ**。
//
// 訂正では期日の天井を超えられないので、置き直しが唯一の延長手段になった。
// 自動化(場外の定期実行)を入れる前に、**人間が見ている前で 1 回試せる**面が要る。
// 初回の実弾操作を無人で走らせない。
func TestPostLiveReplaceProtective(t *testing.T) {
	f := &fakeReplacer{n: 2}
	h := New(nil, nil, nil, nil, nil, nil).WithLiveTrack(&LiveViews{ReplaceProtective: f.Execute})

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/live/protective/replace", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if f.called != 1 {
		t.Errorf("called = %d, want 1", f.called)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v (%s)", err, rec.Body.String())
	}
	if got["replaced"] != float64(2) {
		t.Errorf("replaced = %v, want 2", got["replaced"])
	}
}

// 🚨 失敗は握り潰さない。置き直しの失敗は「建玉が裸」を意味しうるので、
// 本数だけ返して 200 にしてはいけない。
func TestPostLiveReplaceProtectiveSurfacesErrors(t *testing.T) {
	f := &fakeReplacer{n: 0, errs: []string{"🚨 4901: 取消は通ったが再発注に失敗"}}
	h := New(nil, nil, nil, nil, nil, nil).WithLiveTrack(&LiveViews{ReplaceProtective: f.Execute})

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/live/protective/replace", nil))

	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	errs, _ := got["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("errors = %v(握り潰している)", got)
	}
}

// live 未配線なら 503(「取引ゼロ」と区別する)。
func TestPostLiveReplaceProtectiveWithoutLiveTrack(t *testing.T) {
	h := New(nil, nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/live/protective/replace", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// 🛑 **`replaced: 0` の意味を割る。**「期日が近いものが無くて何もしなかった」と
// 「対象はあったが全部失敗した」は運用上まったく別の状態で、前者を後者と読むと
// 毎朝の正常運転が異常に見え、後者を前者と読むと裸を見逃す。
func TestPostLiveReplaceProtectiveReportsSkipped(t *testing.T) {
	f := &fakeReplacer{n: 0, skipped: 3}
	h := New(nil, nil, nil, nil, nil, nil).WithLiveTrack(&LiveViews{ReplaceProtective: f.Execute})

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/live/protective/replace", nil))

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v (%s)", err, rec.Body.String())
	}
	if got["skipped"] != float64(3) {
		t.Errorf("skipped = %v, want 3 — 見送った本数が呼び手に伝わらない", got["skipped"])
	}
}
