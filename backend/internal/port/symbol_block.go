package port

import (
	"context"
	"errors"
	"time"
)

// SymbolBlock は人間が live の新規を止めた 1 銘柄(停止ボタン)。
type SymbolBlock struct {
	Symbol    string
	BlockedAt time.Time
	Note      string
}

// SymbolBlockReader は停止中の銘柄を読む口(発注前ゲートの snapshot と selector が読む)。
//
// 🛑 読めない・壊れているときは error を返す。呼び手はその track の新規を全部止める
// (fail-close。理由は `symbol_blocks_unreadable`)。停止が 0 件なのは error ではない。
type SymbolBlockReader interface {
	List(ctx context.Context) ([]SymbolBlock, error)
}

// SymbolBlockStore は停止と解除の永続化(live だけに配線する。research には無い)。
// 止めるのは新規だけで、解除するまで日をまたいでも再起動しても続く。
type SymbolBlockStore interface {
	SymbolBlockReader
	// Block はその銘柄の新規を止める。既に止まっていれば止めた時刻を保ち、note だけ差し替える。
	Block(ctx context.Context, symbol, note string) error
	// Release は停止を解く。止まっていなければ ErrSymbolNotBlocked。
	Release(ctx context.Context, symbol string) error
}

// ErrSymbolNotBlocked は止まっていない銘柄を解除しようとしたとき。
var ErrSymbolNotBlocked = errors.New("symbol is not blocked")

// IsSymbolBlocked は一覧にその銘柄があるか。
func IsSymbolBlocked(blocks []SymbolBlock, symbol string) bool {
	for _, b := range blocks {
		if b.Symbol == symbol {
			return true
		}
	}
	return false
}
