package command

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/safety"
)

// 🛑 **裸のガードは 1 つの関数を両方の出口が呼ぶ。**
// 決済経路(closeOne)と補償経路(bookCompensatedRoundTrip)はどちらも
// 「守りを cancel してから決済を撃つ」形なので、同じ穴が同じ形で開く。
// 実際 763782c は closeOne だけを塞ぎ、補償経路はそのまま残っていた。
// ロジックを 2 箇所に書けば、次も片方だけ直して緑のまま穴が残る。
func TestNakedGuardIsSharedByBothExitPaths(t *testing.T) {
	callers := map[string]bool{}
	for _, file := range []string{"exit_executor.go", "execute_order.go"} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "tripIfSettleLeavesPositionNaked" {
				callers[file] = true
			}
			return true
		})
	}
	for _, file := range []string{"exit_executor.go", "execute_order.go"} {
		if !callers[file] {
			t.Fatalf("%s が共有ガード tripIfSettleLeavesPositionNaked を呼んでいない — "+
				"守りを cancel した後に決済が板へ載らなければ、その経路の建玉は裸のまま黙る", file)
		}
	}
}

// racySettleBroker は **決済が約定して板から消えた**状態を模す。
//
// 🚨 これを「裸」と読んではいけない。ResolveExecution を撃った時点では未約定でも、
// 数百 ms 後の板照会までに成行返済が約定すれば板からは消える(立花の orderIsActive は
// 全部約定した注文を落とす)。寄り付きの成行返済では普通に起きるレースで、
// ここで鳴らすと **フラットになった建玉のために実弾の新規建てが止まる**。
type racySettleBroker struct {
	*broker.Paper
	fillOnRetry bool // true = 2 回目の照会で「約定していた」と答える
	resolves    int
}

func (b *racySettleBroker) SettleFillsAsync() bool { return true }

func (b *racySettleBroker) ClosePosition(context.Context, port.CloseRequest) (*port.CloseResult, error) {
	return &port.CloseResult{OrderID: "settle-1", Accepted: true}, nil
}

func (b *racySettleBroker) GetActiveOrders(context.Context, string) ([]order.Order, error) {
	return nil, nil // 板は空(約定して消えた / そもそも載らなかった、の区別が付かない)
}

func (b *racySettleBroker) ResolveExecution(_ context.Context, id string) (port.ResolvedExecution, error) {
	if id != "settle-1" {
		return b.Paper.ResolveExecution(context.Background(), id)
	}
	b.resolves++
	if b.fillOnRetry && b.resolves >= 2 {
		return port.ResolvedExecution{OrderID: "settle-1", FilledPrice: 900, FilledQuantity: 100}, nil
	}
	return port.ResolvedExecution{}, fmt.Errorf("settle: %w", port.ErrOrderNotFilled)
}

func nakedGuardFixture(t *testing.T, b port.Broker) (*closeExecutor, position.Position, *safety.EmergencyStop) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 9, 7, 0, 0, clock.JST)
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "shinyo:7203", Symbol: "7203", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 1000, OpenedAt: now,
		Source: position.SourceBot, HoldingMode: order.HoldingMultiday, ExecKind: order.ExecMarginSystem,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = id
	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	x := &closeExecutor{broker: b, posRepo: posRepo, closer: repository.NewCloser(posRepo, trades), emergency: es}
	return x, open[0], es
}

// 🚨 板が空でも **約定して消えただけ**なら鳴らさない。鳴らす直前に約定を引き直す。
// これをやらないと、寄り付きの成行返済が約定するたびに実弾が緊急停止しうる。
func TestNakedGuard_SettleThatFilledAfterTheQueryDoesNotTrip(t *testing.T) {
	ctx := context.Background()
	pb := broker.NewPaper(clock.Fixed(time.Now()), 0, 0)
	pb.SetPrice("7203", 1000)
	b := &racySettleBroker{Paper: pb, fillOnRetry: true}
	x, p, es := nakedGuardFixture(t, b)

	if ok, _ := x.closeOne(ctx, p, 1000, "manual", time.Now()); ok {
		t.Fatal("precondition: 初回の照会では約定を確認できない")
	}
	if es.Active() {
		t.Fatalf("約定して板から消えただけの決済で緊急停止している: %q — "+
			"板が空 = 裸、と即断せず約定を引き直すこと", es.Reason())
	}
	if b.resolves < 2 {
		t.Fatalf("鳴らす前に約定を引き直していない(resolves=%d)", b.resolves)
	}
}

// 逆に、引き直しても約定していないなら鳴らす。**引き直しは誤爆を減らす手段であって、
// 裸を見逃す言い訳にしてはいけない。**
func TestNakedGuard_StillUnfilledAfterRetryTrips(t *testing.T) {
	ctx := context.Background()
	pb := broker.NewPaper(clock.Fixed(time.Now()), 0, 0)
	pb.SetPrice("7203", 1000)
	b := &racySettleBroker{Paper: pb, fillOnRetry: false}
	x, p, es := nakedGuardFixture(t, b)

	if ok, _ := x.closeOne(ctx, p, 1000, "manual", time.Now()); ok {
		t.Fatal("precondition: 約定は確認できない")
	}
	if !es.Active() {
		t.Fatal("引き直しても未約定で板にも無い = 本当に裸。鳴らさねばならない")
	}
}
