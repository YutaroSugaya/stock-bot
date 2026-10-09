package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

type armBoardStub struct {
	orders []port.ProtectiveOrderInfo
	placed *port.OCOCloseOrderInput
}

func (b *armBoardStub) ListProtectiveOrders(context.Context, string) ([]port.ProtectiveOrderInfo, error) {
	return b.orders, nil
}

func (b *armBoardStub) PlaceSettleOCO(_ context.Context, in port.OCOCloseOrderInput) (string, error) {
	b.placed = &in
	return "armed-1", nil
}

func armFixture(t *testing.T, status position.Status, brk *armBoardStub) *ArmProtectiveOrder {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, clock.JST)
	repo := repository.NewInMemoryPositionRepo()
	id, err := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "shinyo:4704", Symbol: "4704", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 5381, OpenedAt: now,
		Source: position.SourceBot, HoldingMode: order.HoldingMultiday, ExecKind: order.ExecMarginSystem,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if status == position.StatusClosing {
		if ok, err := repo.ClaimForClose(ctx, id, now); err != nil || !ok {
			t.Fatalf("claim: %v", err)
		}
	}
	return NewArmProtectiveOrder(repo, brk, tokyoHours(), clock.Fixed(now))
}

// 🚨 **bot が自力で裸にした状態を bot が直せない**、が構造的な
// 理由になる。守りを cancel した後に決済が板へ載らないと建玉は CLOSING かつ裸になるが、
// arm は CLOSING を弾いていたので復旧の口が閉じていた。板のスイープが
// 「arm で復旧せよ」と案内する状態を arm が拒否する、という自己矛盾でもあった。
// 結果、人間が証券アプリで直すしかなくなる。
func TestArmProtective_CanArmANakedClosingPosition(t *testing.T) {
	ctx := context.Background()
	brk := &armBoardStub{} // 板は空 = 守りも決済注文も無い
	a := armFixture(t, position.StatusClosing, brk)

	id, err := a.Execute(ctx, ArmProtectiveInput{Symbol: "4704", TakeProfit: 6000, StopLoss: 5000})
	if err != nil {
		t.Fatalf("決済中で裸の建玉に守りを置けない: %v — bot が自力で裸にした状態を直せない", err)
	}
	if id == "" || brk.placed == nil {
		t.Fatal("守りが発注されていない")
	}
	if brk.placed.Quantity != 100 || brk.placed.ExecKind != order.ExecMarginSystem {
		t.Fatalf("台帳の凍結値を使っていない: %+v", *brk.placed)
	}
}

// 🛑 ただし **板に決済側の注文が残っていれば置かない**。決済が進行中の建玉に
// 返済注文を重ねると実質の新規売りになりうる。
// 逆指値脚の有無で絞らないこと —— 成行の返済注文は脚を持たない。
func TestArmProtective_RefusesWhenASettleOrderIsStillResting(t *testing.T) {
	ctx := context.Background()
	brk := &armBoardStub{orders: []port.ProtectiveOrderInfo{{
		OrderID: "settle-1", Symbol: "4704", Side: order.SideSell,
		Quantity: 100, HasStopLeg: false, // 成行の返済注文
	}}}
	a := armFixture(t, position.StatusClosing, brk)

	_, err := a.Execute(ctx, ArmProtectiveInput{Symbol: "4704", TakeProfit: 6000, StopLoss: 5000})
	if err == nil {
		t.Fatal("決済が板で進行中なのに守りを重ねた — 実質の新規売りになりうる")
	}
	if !strings.Contains(err.Error(), "決済中") {
		t.Fatalf("理由が読み取れない: %v", err)
	}
	if brk.placed != nil {
		t.Fatal("拒否したのに発注している")
	}
}

// OPEN の建玉は従来どおり arm できる(回帰ガード)。
func TestArmProtective_StillArmsAnOpenPosition(t *testing.T) {
	ctx := context.Background()
	brk := &armBoardStub{}
	a := armFixture(t, position.StatusOpen, brk)

	if _, err := a.Execute(ctx, ArmProtectiveInput{Symbol: "4704", TakeProfit: 6000, StopLoss: 5000}); err != nil {
		t.Fatalf("OPEN の裸建玉に守りを置けない: %v", err)
	}
}
