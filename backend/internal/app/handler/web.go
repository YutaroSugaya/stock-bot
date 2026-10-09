package handler

import (
	"embed"
	"net/http"
)

//go:embed web/index.html
var webFS embed.FS

// dashboard は zero-build の静的 SPA(素の JS)。bot 全体を Go バイナリ1つで
// 配れるようにするため — node のビルド手順も2つ目のプロセスも増やさない。
func (h *Handler) indexPage(w http.ResponseWriter, _ *http.Request) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "dashboard not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}
