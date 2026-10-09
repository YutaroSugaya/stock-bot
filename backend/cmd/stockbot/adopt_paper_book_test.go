package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/testutil"
)

// 再起動を跨いだ建玉は、紙の帳簿にも復元されていないと決済できない。
func TestAdoptPaperBookRestoresOpenPositions(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	id, err := repo.Insert(ctx, port.PositionInsertInput{
		Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 3000,
		BrokerPositionID: "bp-2", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	_ = id

	pb := broker.NewPaper(clock.System(), 0, 0)
	pb.SetPrice("7203", 3100)
	if err := adoptPaperBook(ctx, pb, repo, testutil.SilentLogger()); err != nil {
		t.Fatalf("adoptPaperBook: %v", err)
	}
	res, err := pb.ClosePosition(ctx, port.CloseRequest{
		Symbol: "7203", BrokerPositionID: "bp-2", Side: order.SideSell, Quantity: 100})
	if err != nil || !res.Accepted {
		t.Fatalf("復元されていない(再起動後に TP/SL が死ぬ): %+v %v", res, err)
	}
}

// stubLiveBroker は「建玉の正本が自分側にある」実ブローカー(立花)の代役。
// MOCK rationale: system boundary (broker adapter)。
//
// 🛑 埋め込みは nil のまま**呼ばない**前提。adoptPaperBook は `brk.(paperBook)` の
// 型アサーションに失敗した時点で即 return するので、promote されたメソッドは 1 本も
// 呼ばれない。`port.LiveBroker` に AdoptOpenPositions は無いので、埋め込んでも
// paperBook を満たさない = この代役が「実ブローカー」であることは型で保たれる。
type stubLiveBroker struct{ port.LiveBroker }

// paper 以外(立花)は自分が建玉の正本なので復元しない — no-op で通す。
func TestAdoptPaperBookIsNoOpForNonPaperBroker(t *testing.T) {
	if err := adoptPaperBook(context.Background(), stubLiveBroker{}, repository.NewInMemoryPositionRepo(), testutil.SilentLogger()); err != nil {
		t.Fatalf("非 paper broker で error: %v", err)
	}
}

// broker_position_id が台帳の中で重複していたら起動を止める(fail-close)。重複した
// まま走ると ClosePosition が**別銘柄の建玉**を閉じ、その値段で trades に記録される。
func TestAdoptPaperBookRejectsDuplicateBrokerPositionIDs(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	for _, sym := range []string{"7203", "6758"} {
		if _, err := repo.Insert(ctx, port.PositionInsertInput{
			Symbol: sym, Side: order.SideBuy, Quantity: 100, EntryPrice: 3000,
			BrokerPositionID: "bp-2", OpenedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Insert %s: %v", sym, err)
		}
	}
	pb := broker.NewPaper(clock.System(), 0, 0)
	err := adoptPaperBook(ctx, pb, repo, testutil.SilentLogger())
	if err == nil {
		t.Fatal("重複 id を黙って受け入れた(別銘柄の建玉を決済して台帳が汚れる)")
	}
	for _, want := range []string{"bp-2", "7203", "6758"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message に %q が無い(直せない): %v", want, err)
		}
	}
}
