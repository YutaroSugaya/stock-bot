package handler

import (
	"net/http"
	"time"

	"stockbot/backend/internal/usecase/query"
)

// PerformanceCycle は戦績の切り替え単位(期間ごと・全期間)。
// paper は期間ごとに DB を切れるので「どの台帳を・どの期間で」の組で持つ。
//
// Report nil = そのトラックの既定の台帳(`WithPerformance` / `LiveViews.Performance`)を
// 期間で切る。Since / Until の zero は下限 / 上限なし。Until は排他(その日の 0 時 JST)。
type PerformanceCycle struct {
	Key    string
	Label  string
	Since  time.Time
	Until  time.Time
	Report *query.BuildForwardReport
}

type performanceCycleItem struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type performanceCycleList struct {
	Default string                 `json:"default"`
	Cycles  []performanceCycleItem `json:"cycles"`
}

// WithPerformanceCycles は research トラックの戦績サイクルを注入する。def は画面の既定。
func (h *Handler) WithPerformanceCycles(def string, cycles []PerformanceCycle) *Handler {
	h.perfCycleDefault, h.perfCycles = def, cycles
	return h
}

func (h *Handler) getPerformanceCycles(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, cycleList(h.perfCycleDefault, h.perfCycles), nil)
}

func (h *Handler) getLivePerformanceCycles(v *LiveViews, w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, cycleList(v.CycleDefault, v.Cycles), nil)
}

func cycleList(def string, cycles []PerformanceCycle) performanceCycleList {
	out := performanceCycleList{Default: def, Cycles: make([]performanceCycleItem, 0, len(cycles))}
	for _, c := range cycles {
		out.Cycles = append(out.Cycles, performanceCycleItem{Key: c.Key, Label: c.Label})
	}
	return out
}

// servePerformance は research / live 共通の本体。`?cycle=` があればサイクルで、無ければ
// 従来どおり `?since=`(JST 0 時)で切る。知らないサイクル・壊れた since は黙って全期間に
// 落とさず 400 — 別物の集計を戦績として見せない。
func servePerformance(w http.ResponseWriter, r *http.Request, base *query.BuildForwardReport, cycles []PerformanceCycle) {
	if key := r.URL.Query().Get("cycle"); key != "" {
		for _, c := range cycles {
			if c.Key != key {
				continue
			}
			rep := c.Report
			if rep == nil {
				rep = base
			}
			view, err := rep.ExecuteRange(r.Context(), c.Since, c.Until)
			writeJSON(w, view, err)
			return
		}
		http.Error(w, "unknown cycle: "+key, http.StatusBadRequest)
		return
	}
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, jst)
		if err != nil {
			http.Error(w, "since must be YYYY-MM-DD (JST)", http.StatusBadRequest)
			return
		}
		since = t
	}
	view, err := base.Execute(r.Context(), since)
	writeJSON(w, view, err)
}
