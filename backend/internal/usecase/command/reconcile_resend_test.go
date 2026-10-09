package command

import (
	"context"
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

// resendBoardStub は再送した**成行**の返済注文が板に残る broker。
// 成行の返済注文は **逆指値脚を持たない** ので、HasStopLeg を要求する述語では
// 自分の注文を毎回「板に無い」と読んでしまう。
type resendBoardStub struct {
	*broker.Paper
	board        []order.Order
	settleTries  int
	restAfterOne bool // true = 1 回目の再送でその注文が板に残る
}

func (b *resendBoardStub) SettleFillsAsync() bool { return true }

func (b *resendBoardStub) ClosePosition(_ context.Context, req port.CloseRequest) (*port.CloseResult, error) {
	b.settleTries++
	if b.restAfterOne {
		b.board = append(b.board, order.Order{
			OrderID: "resend-1", Symbol: req.Symbol, Side: req.Side,
			Quantity: req.Quantity, Status: "working", // HasStopLeg=false = 成行
		})
	}
	return &port.CloseResult{OrderID: "resend-1", Accepted: true}, nil
}

func (b *resendBoardStub) ResolveExecution(_ context.Context, id string) (port.ResolvedExecution, error) {
	if id == "resend-1" {
		return port.ResolvedExecution{}, port.ErrOrderNotFilled
	}
	return b.Paper.ResolveExecution(context.Background(), id)
}

func (b *resendBoardStub) GetActiveOrders(context.Context, string) ([]order.Order, error) {
	return b.board, nil
}

// GetPositions は broker がまだその建玉を持っている状態(= CLOSING の解決対象)。
func (b *resendBoardStub) GetPositions(context.Context) ([]port.BrokerPosition, error) {
	return []port.BrokerPosition{{
		BrokerPositionID: "bp-1", Symbol: "7203", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 1000,
	}}, nil
}

func stuckClosingFixture(t *testing.T, b port.Broker) (*Reconcile, *safety.EmergencyStop, port.PositionRepository) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, clock.JST)
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "bp-1", Symbol: "7203", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 1000, OpenedAt: now,
		Source: position.SourceBot, HoldingMode: order.HoldingMultiday, ExecKind: order.ExecMarginSystem,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if ok, err := posRepo.ClaimForClose(ctx, id, now); err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	rec := NewReconcile(b, posRepo, repository.NewCloser(posRepo, trades),
		safety.NewPendingPositions(), es, clock.Fixed(now))
	rec.gracePeriod = 0 // 猶予を超えた状態
	return rec, es, posRepo
}

// 🚨 再送した**成行**の返済注文は逆指値脚を持たない。述語が HasStopLeg を要求すると
// 自分の注文を毎ラウンド「板に無い」と読み、**再送し続ける**。live の reconcile 間隔は
// 1 時間で hardPeriod は 10 分なので 2 巡目は必ず age >= hardPeriod。再送が拘束で
// 拒否されると close_stuck_unresolved を trip する —— それは exit_executor が
// 「trip してはいけない」と明記した **ストップ安の比例配分待ち**そのもの。
func TestReconcile_RestingMarketSettleIsNotResent(t *testing.T) {
	ctx := context.Background()
	pb := broker.NewPaper(clock.Fixed(time.Now()), 0, 0)
	pb.SetPrice("7203", 1000)
	b := &resendBoardStub{Paper: pb, restAfterOne: true}
	rec, es, _ := stuckClosingFixture(t, b)

	for round := 1; round <= 3; round++ {
		if _, err := rec.Run(ctx, "7203"); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if b.settleTries != 1 {
		t.Fatalf("板に残っている自分の成行を毎回「無い」と読んで再送している(%d 回)— "+
			"成行の返済注文は逆指値脚を持たないので HasStopLeg で絞ってはいけない", b.settleTries)
	}
	if es.Active() {
		t.Fatalf("比例配分を待っているだけの状態で緊急停止している: %q", es.Reason())
	}
}

// 🚨 板に**何も無い**まま再送も約定しないなら、その建玉は逆指値も決済注文も持たない
// = 裸。closeOne が 763782c で塞いだ穴が、1 時間後の再送枝にそのまま残っていた。
func TestReconcile_ResendThatDoesNotRestTripsAsNaked(t *testing.T) {
	ctx := context.Background()
	pb := broker.NewPaper(clock.Fixed(time.Now()), 0, 0)
	pb.SetPrice("7203", 1000)
	b := &resendBoardStub{Paper: pb, restAfterOne: false} // 板に載らない
	rec, es, _ := stuckClosingFixture(t, b)

	if _, err := rec.Run(ctx, "7203"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !es.Active() {
		t.Fatal("守りは cancel 済みで再送も板に載っていない = 裸。鳴らさねばならない")
	}
}

// 🚨 **他人の端数注文を「自分の全量が板にある」と読まない。**
// 決済注文 id を持たないので誰の注文かは区別できない。数量条件が無いと、人間が
// 置いた 50 株の売り注文で「決済注文が働いている」と読み、守りが cancel 済みの
// 建玉が **裸のまま永久に defer** する。実口座で 4704 は 100株の建玉に
// 決済注文が 200株 載っていた — 板に他人の注文が載るのは実測済みの前提。
func TestReconcile_ForeignPartialOrderDoesNotCountAsOurSettle(t *testing.T) {
	ctx := context.Background()
	pb := broker.NewPaper(clock.Fixed(time.Now()), 0, 0)
	pb.SetPrice("7203", 1000)
	b := &resendBoardStub{Paper: pb, restAfterOne: false}
	b.board = []order.Order{{ // 人間が置いた 50 株の売り指値(建玉は 100 株)
		OrderID: "human-1", Symbol: "7203", Side: order.SideSell,
		Quantity: 50, Status: "working",
	}}
	rec, es, _ := stuckClosingFixture(t, b)

	if _, err := rec.Run(ctx, "7203"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if b.settleTries == 0 && !es.Active() {
		t.Fatal("他人の端数注文を自分の決済と読んで、裸のまま黙って滞留している — " +
			"数量は >= 建玉数量 を要求すること")
	}
}

// 🛑 板に決済側の注文が見え続けても、**待ちには上限を置く**。
// 見えている注文が自分のものである保証は無く(id を持たない)、守りは cancel 済み。
// 上限が無いと「裸のまま静かに滞留する」状態を新設することになる。
func TestReconcile_RestingSettleEscalatesAfterTheLimit(t *testing.T) {
	ctx := context.Background()
	pb := broker.NewPaper(clock.Fixed(time.Now()), 0, 0)
	pb.SetPrice("7203", 1000)
	b := &resendBoardStub{Paper: pb, restAfterOne: true}
	rec, es, _ := stuckClosingFixture(t, b)

	if _, err := rec.Run(ctx, "7203"); err != nil { // 1 巡目: 再送して板に載る
		t.Fatalf("reconcile: %v", err)
	}
	rec.restingLimit = 0 // 上限を超えた状態
	if _, err := rec.Run(ctx, "7203"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !es.Active() {
		t.Fatal("守りが無いまま上限を超えて滞留している。人間を呼ばねばならない")
	}
}
