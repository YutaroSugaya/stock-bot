package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/command"
	"stockbot/backend/internal/usecase/query"
)

type handlerBlocks struct{ blocks map[string]string }

func (m *handlerBlocks) List(context.Context) ([]port.SymbolBlock, error) {
	var out []port.SymbolBlock
	for s, n := range m.blocks {
		out = append(out, port.SymbolBlock{Symbol: s, Note: n})
	}
	return out, nil
}
func (m *handlerBlocks) Block(_ context.Context, s, n string) error { m.blocks[s] = n; return nil }
func (m *handlerBlocks) Release(_ context.Context, s string) error {
	if _, ok := m.blocks[s]; !ok {
		return port.ErrSymbolNotBlocked
	}
	delete(m.blocks, s)
	return nil
}

func symbolBlockHandler(m *handlerBlocks) http.Handler {
	allowed := func(s string) bool { return s == "6594" || s == "7203" }
	return New(nil, nil, nil, nil, nil, nil).WithLiveTrack(&LiveViews{
		BlockSymbol:   command.NewBlockLiveSymbol(m, allowed),
		ReleaseSymbol: command.NewReleaseLiveSymbol(m),
		SymbolBlocks:  query.NewListSymbolBlocks(m),
	}).Routes()
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

func TestPostLiveSymbolBlock(t *testing.T) {
	m := &handlerBlocks{blocks: map[string]string{}}
	h := symbolBlockHandler(m)
	rec := post(h, "/api/live/symbol-blocks", `{"symbol":"6594","note":"会計不正"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if m.blocks["6594"] != "会計不正" {
		t.Fatalf("止まっていない: %v", m.blocks)
	}
	var got query.SymbolBlocksView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Blocks) != 1 {
		t.Fatalf("応答は停止の一覧: %s", rec.Body.String())
	}
}

func TestPostLiveSymbolBlock_RejectsSymbolOutsideWhitelist(t *testing.T) {
	m := &handlerBlocks{blocks: map[string]string{}}
	if rec := post(symbolBlockHandler(m), "/api/live/symbol-blocks", `{"symbol":"0000"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("allowed_symbols の外: status = %d", rec.Code)
	}
	if rec := post(symbolBlockHandler(m), "/api/live/symbol-blocks", `not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("壊れた body: status = %d", rec.Code)
	}
	if len(m.blocks) != 0 {
		t.Fatalf("止めてはいけない: %v", m.blocks)
	}
}

func TestPostLiveSymbolBlockRelease(t *testing.T) {
	m := &handlerBlocks{blocks: map[string]string{"6594": ""}}
	h := symbolBlockHandler(m)
	if rec := post(h, "/api/live/symbol-blocks/release", `{"symbol":"6594"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if len(m.blocks) != 0 {
		t.Fatalf("解除されていない: %v", m.blocks)
	}
	if rec := post(h, "/api/live/symbol-blocks/release", `{"symbol":"6594"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("止まっていない銘柄の解除: status = %d", rec.Code)
	}
}

// 制御 POST と同じく、ブラウザからの別オリジンの書き込みは通さない(guardWrite)。
func TestPostLiveSymbolBlock_GuardedLikeOtherControlPosts(t *testing.T) {
	m := &handlerBlocks{blocks: map[string]string{}}
	req := httptest.NewRequest(http.MethodPost, "/api/live/symbol-blocks", strings.NewReader(`{"symbol":"6594"}`))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	symbolBlockHandler(m).ServeHTTP(rec, req)
	if rec.Code == http.StatusOK || len(m.blocks) != 0 {
		t.Fatalf("別オリジンの書き込みが通った: status = %d", rec.Code)
	}
}

func TestPostLiveSymbolBlock_WithoutLiveTrack(t *testing.T) {
	if rec := post(New(nil, nil, nil, nil, nil, nil).Routes(), "/api/live/symbol-blocks", `{"symbol":"6594"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
