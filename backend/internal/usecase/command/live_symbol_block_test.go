package command

import (
	"context"
	"errors"
	"testing"

	"stockbot/backend/internal/port"
)

type memBlocks struct {
	blocks map[string]string
	err    error
}

func (m *memBlocks) List(context.Context) ([]port.SymbolBlock, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []port.SymbolBlock
	for s, n := range m.blocks {
		out = append(out, port.SymbolBlock{Symbol: s, Note: n})
	}
	return out, nil
}

func (m *memBlocks) Block(_ context.Context, sym, note string) error {
	if m.err != nil {
		return m.err
	}
	m.blocks[sym] = note
	return nil
}

func (m *memBlocks) Release(_ context.Context, sym string) error {
	if _, ok := m.blocks[sym]; !ok {
		return port.ErrSymbolNotBlocked
	}
	delete(m.blocks, sym)
	return nil
}

func allowOnly(syms ...string) func(string) bool {
	return func(s string) bool {
		for _, x := range syms {
			if x == s {
				return true
			}
		}
		return false
	}
}

func TestBlockLiveSymbol_OnlyWhitelistedSymbols(t *testing.T) {
	m := &memBlocks{blocks: map[string]string{}}
	cmd := NewBlockLiveSymbol(m, allowOnly("6594"))
	if err := cmd.Execute(context.Background(), "6594", "会計不正"); err != nil {
		t.Fatalf("block: %v", err)
	}
	if m.blocks["6594"] != "会計不正" {
		t.Fatalf("止まっていない: %v", m.blocks)
	}
	if err := cmd.Execute(context.Background(), "0000", ""); !errors.Is(err, ErrSymbolNotAllowed) {
		t.Fatalf("allowed_symbols の外を受けた: %v", err)
	}
	if err := cmd.Execute(context.Background(), " ", ""); !errors.Is(err, ErrSymbolNotAllowed) {
		t.Fatalf("空の銘柄を受けた: %v", err)
	}
}

func TestReleaseLiveSymbol(t *testing.T) {
	m := &memBlocks{blocks: map[string]string{"6594": ""}}
	cmd := NewReleaseLiveSymbol(m)
	if err := cmd.Execute(context.Background(), "6594"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := cmd.Execute(context.Background(), "6594"); !errors.Is(err, port.ErrSymbolNotBlocked) {
		t.Fatalf("止まっていない銘柄の解除: %v", err)
	}
}
