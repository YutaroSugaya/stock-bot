package command

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"stockbot/backend/internal/port"
)

// ErrSymbolNotAllowed は allowed_symbols(人間 commit のホワイトリスト)に無い銘柄を止めようとしたとき。
var ErrSymbolNotAllowed = errors.New("symbol is not in allowed_symbols")

// BlockLiveSymbol は live のその銘柄の**新規だけ**を止める(人間のボタン)。
//
// 止めた銘柄は 2 か所で落ちる: selector が arm しない(arm 済みなら次の Tick で外す)/
// 発注直前のゲートが `manual_symbol_block` で reject する。決済・守り・引け前フラット化・
// max_hold 延長・既存の建玉には効かない。解除するまで日をまたいでも再起動しても続く。
type BlockLiveSymbol struct {
	store   port.SymbolBlockStore
	allowed func(string) bool
}

func NewBlockLiveSymbol(store port.SymbolBlockStore, allowed func(string) bool) *BlockLiveSymbol {
	return &BlockLiveSymbol{store: store, allowed: allowed}
}

func (c *BlockLiveSymbol) Execute(ctx context.Context, symbol, note string) error {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" || c.allowed == nil || !c.allowed(symbol) {
		return fmt.Errorf("%w: %q", ErrSymbolNotAllowed, symbol)
	}
	return c.store.Block(ctx, symbol, strings.TrimSpace(note))
}

// ReleaseLiveSymbol は停止を解く。解除すれば次の発火で建つ。
type ReleaseLiveSymbol struct {
	store port.SymbolBlockStore
}

func NewReleaseLiveSymbol(store port.SymbolBlockStore) *ReleaseLiveSymbol {
	return &ReleaseLiveSymbol{store: store}
}

func (c *ReleaseLiveSymbol) Execute(ctx context.Context, symbol string) error {
	return c.store.Release(ctx, strings.TrimSpace(symbol))
}
