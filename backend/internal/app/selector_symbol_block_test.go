package app

import (
	"context"
	"errors"
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

type selBlocks struct {
	blocks []port.SymbolBlock
	err    error
}

func (s *selBlocks) List(context.Context) ([]port.SymbolBlock, error) { return s.blocks, s.err }

// 止めた銘柄は arm しない。枠は消費しない(下位の候補が繰り上がる)。
func TestSelector_DoesNotArmBlockedSymbol(t *testing.T) {
	sel, holders, _ := newSelectorFixtureMulti(t, 2)
	sel.WithSymbolBlocks(&selBlocks{blocks: []port.SymbolBlock{{Symbol: "7203"}}})
	sel.Tick(context.Background())
	if g := holders["7203"].Get(); g.StrategyName != config.StrategyNoTrade {
		t.Fatalf("止めた 7203 が arm された: %s", g.StrategyName)
	}
	if n := armedCount(holders, "6758", "9984"); n != 2 {
		t.Fatalf("止めた銘柄が枠を消費した: armed=%d", n)
	}
}

// 既に arm 済みの銘柄は、止めた後の次の Tick で外す。
func TestSelector_DisarmsSymbolBlockedAfterArming(t *testing.T) {
	sel, holders, _ := newSelectorFixtureMulti(t, 3)
	b := &selBlocks{}
	sel.WithSymbolBlocks(b)
	sel.Tick(context.Background())
	if g := holders["7203"].Get(); g.StrategyName != config.StrategyBNFReversion {
		t.Fatalf("前提: 7203 は arm される: %s", g.StrategyName)
	}
	b.blocks = []port.SymbolBlock{{Symbol: "7203"}}
	sel.Tick(context.Background())
	if g := holders["7203"].Get(); g.StrategyName != config.StrategyNoTrade {
		t.Fatalf("止めた後も arm されたまま: %s", g.StrategyName)
	}
}

// 建玉中の銘柄は止めても disarm しない(config 凍結・決済と守りは止めない)。
func TestSelector_BlockNeverDisarmsHeldSymbol(t *testing.T) {
	sel, holders, pos := newSelectorFixtureMulti(t, 3)
	held := &config.StrategyConfig{ConfigID: "armed_bnf_7203", Symbol: "7203", StrategyName: config.StrategyBNFReversion, HoldingMode: config.HoldingMultiday}
	holders["7203"].Set(held)
	if _, err := pos.Insert(context.Background(), port.PositionInsertInput{Symbol: "7203", Side: order.SideBuy, Quantity: 100}); err != nil {
		t.Fatal(err)
	}
	sel.WithSymbolBlocks(&selBlocks{blocks: []port.SymbolBlock{{Symbol: "7203"}}})
	sel.Tick(context.Background())
	if g := holders["7203"].Get(); g.ConfigID != held.ConfigID {
		t.Fatalf("建玉中の銘柄を disarm した: %s", g.ConfigID)
	}
}

// 停止のファイルが読めないときは 1 銘柄も arm しない(fail-close)。
func TestSelector_UnreadableBlocksArmsNothing(t *testing.T) {
	sel, holders, _ := newSelectorFixtureMulti(t, 3)
	sel.WithSymbolBlocks(&selBlocks{err: errors.New("broken")})
	sel.Tick(context.Background())
	if n := armedCount(holders, "6758", "7203", "9984"); n != 0 {
		t.Fatalf("読めないのに arm した: %d", n)
	}
}
