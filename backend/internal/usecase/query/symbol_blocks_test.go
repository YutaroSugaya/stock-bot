package query

import (
	"context"
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/port"
)

type stubBlockReader struct {
	blocks []port.SymbolBlock
	err    error
}

func (s stubBlockReader) List(context.Context) ([]port.SymbolBlock, error) { return s.blocks, s.err }

func TestListSymbolBlocks_View(t *testing.T) {
	at := time.Date(2026, 10, 2, 8, 30, 0, 0, time.FixedZone("JST", 9*3600))
	v := NewListSymbolBlocks(stubBlockReader{blocks: []port.SymbolBlock{{Symbol: "6594", BlockedAt: at, Note: "n"}}}).
		Execute(context.Background())
	if v.Error != "" || len(v.Blocks) != 1 || v.Blocks[0].Symbol != "6594" || v.Blocks[0].Note != "n" ||
		v.Blocks[0].BlockedAt != "2026-10-02T08:30:00+09:00" {
		t.Fatalf("view: %+v", v)
	}
}

// 読めないときは画面に出す(live の新規は全部止まっている)。一覧は空でも nil にしない。
func TestListSymbolBlocks_UnreadableIsShown(t *testing.T) {
	v := NewListSymbolBlocks(stubBlockReader{err: errors.New("parse error")}).Execute(context.Background())
	if v.Error == "" || v.Blocks == nil {
		t.Fatalf("読めない状態が画面に出ない: %+v", v)
	}
}
