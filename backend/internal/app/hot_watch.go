package app

import "sync"

// HotWatch は段階ウォッチ Tier A の共有レジストリ(前日確定足が BNF パニックの
// 銘柄をその日だけ短周期ポーリングへ上げる)。各 SymbolBundle が自分の price
// goroutine から1日1回書き、バッチフィードと price loop が並行に読むので lock。
type HotWatch struct {
	mu  sync.RWMutex
	set map[string]bool
}

func NewHotWatch() *HotWatch {
	return &HotWatch{set: map[string]bool{}}
}

func (h *HotWatch) Set(symbol string, hot bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hot {
		h.set[symbol] = true
	} else {
		delete(h.set, symbol)
	}
}

// nil-safe: レジストリを作らない配線(素の paper / テスト)は「全部 cold」に倒す。
func (h *HotWatch) IsHot(symbol string) bool {
	if h == nil {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.set[symbol]
}
