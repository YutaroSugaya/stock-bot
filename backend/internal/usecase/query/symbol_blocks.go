package query

import (
	"context"
	"time"

	"stockbot/backend/internal/port"
)

// SymbolBlockView は live の停止中の 1 銘柄(画面用)。
type SymbolBlockView struct {
	Symbol    string `json:"symbol"`
	BlockedAt string `json:"blocked_at"`
	Note      string `json:"note"`
}

// SymbolBlocksView は live の銘柄ごとの新規停止の一覧。
// Error が空でなければ停止のファイルが読めず、**live の新規は全部止まっている**(fail-close)。
type SymbolBlocksView struct {
	Blocks []SymbolBlockView `json:"blocks"`
	Error  string            `json:"error,omitempty"`
}

// ListSymbolBlocks は停止中の銘柄を live dashboard に載せる。
type ListSymbolBlocks struct {
	reader port.SymbolBlockReader
}

func NewListSymbolBlocks(r port.SymbolBlockReader) *ListSymbolBlocks {
	return &ListSymbolBlocks{reader: r}
}

func (q *ListSymbolBlocks) Execute(ctx context.Context) SymbolBlocksView {
	v := SymbolBlocksView{Blocks: []SymbolBlockView{}}
	blocks, err := q.reader.List(ctx)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	for _, b := range blocks {
		v.Blocks = append(v.Blocks, SymbolBlockView{Symbol: b.Symbol, BlockedAt: b.BlockedAt.Format(time.RFC3339), Note: b.Note})
	}
	return v
}
