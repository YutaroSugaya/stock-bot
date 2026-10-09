package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"stockbot/backend/internal/usecase/query"
)

type fakeExtendOptions struct {
	gotID int64
	view  *query.ExtendOptionsView
}

func (f *fakeExtendOptions) Execute(_ context.Context, id int64) (*query.ExtendOptionsView, error) {
	f.gotID = id
	return f.view, nil
}

// 延長先の候補(各営業日の引け前)は**サーバが出す** — 休場日を知っているのは
// カレンダーだけで、画面は選ばれた候補の add_minutes を既存の延長 API へ渡すだけ。
func TestGetLiveExtendOptions(t *testing.T) {
	until := time.Date(2026, 10, 2, 5, 50, 10, 0, time.UTC)
	f := &fakeExtendOptions{view: &query.ExtendOptionsView{
		PositionID: 3, Symbol: "4901", MaxAddMinutes: 43200,
		Options: []query.ExtendOption{{Until: until, AddMinutes: 1440, BusinessDays: 1}},
	}}
	h := New(nil, nil, nil, nil, nil, nil).WithLiveTrack(&LiveViews{ExtendOptions: f})

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/live/positions/extend-options?position_id=3", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if f.gotID != 3 {
		t.Errorf("position_id = %d, want 3", f.gotID)
	}
	var got query.ExtendOptionsView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v (%s)", err, rec.Body.String())
	}
	if len(got.Options) != 1 || got.Options[0].AddMinutes != 1440 || !got.Options[0].Until.Equal(until) {
		t.Errorf("options = %+v", got.Options)
	}
}

func TestGetLiveExtendOptionsErrors(t *testing.T) {
	cases := []struct {
		name string
		lv   *LiveViews
		path string
		want int
	}{
		{"未配線は 503", &LiveViews{}, "/api/live/positions/extend-options?position_id=3", http.StatusServiceUnavailable},
		{"id 無しは 400", &LiveViews{ExtendOptions: &fakeExtendOptions{}}, "/api/live/positions/extend-options", http.StatusBadRequest},
		{"id が数でないなら 400", &LiveViews{ExtendOptions: &fakeExtendOptions{}}, "/api/live/positions/extend-options?position_id=abc", http.StatusBadRequest},
		{"OPEN でない / 未知 id は 404", &LiveViews{ExtendOptions: &fakeExtendOptions{}}, "/api/live/positions/extend-options?position_id=9", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := New(nil, nil, nil, nil, nil, nil).WithLiveTrack(c.lv)
			rec := httptest.NewRecorder()
			h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}
