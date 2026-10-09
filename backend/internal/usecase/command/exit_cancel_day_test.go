package command

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

// dayKeyedBroker は立花のように **営業日つきでしか守りを取り消せない** broker。
//
// 立花の取消は (注文番号, 営業日) で引く。営業日を運ばない CancelOrder(id) は
// プロセス内 map に頼るので、前のプロセスが出した守り(= 多日保有の逆指値)には
// 必ず外れる。ここではその性質を「CancelOrder は守りに対して必ず失敗する /
// CancelProtectiveOrder は BrokerRef があれば成功する」として模す。
type dayKeyedBroker struct {
	*broker.Paper
	resting    []port.ProtectiveOrderInfo
	listErr    error
	cancelled  []string
	byPlainID  []string // CancelOrder で撃たれた id(= 営業日を運べていない経路)
	rejectMsg  string
	closeCalls int
}

// 🛑 **broker はこの建玉を持っている。**「返済可能数量を超えています」も
// 「区分が不一致」も、**建玉が在るからこそ**返ってくる拒否である。fixture が
// 持っていないと、closeOne の「拒否されたら建玉照会で確かめる」枝が
// 「もう決済済み」に倒れ、**裸を見逃すバグが緑のまま通る**。
func (b *dayKeyedBroker) GetPositions(context.Context) ([]port.BrokerPosition, error) {
	return []port.BrokerPosition{{
		BrokerPositionID: "shinyo:7203", Symbol: "7203", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 1000, ExecKind: order.ExecMarginSystem,
	}}, nil
}

func (b *dayKeyedBroker) ListProtectiveOrders(context.Context, string) ([]port.ProtectiveOrderInfo, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	return b.resting, nil
}

func (b *dayKeyedBroker) CancelProtectiveOrder(_ context.Context, o port.ProtectiveOrderInfo) error {
	if o.BrokerRef == "" {
		return fmt.Errorf("営業日が無いので撃たない(order %s)", o.OrderID)
	}
	b.cancelled = append(b.cancelled, o.OrderID)
	return nil
}

func (b *dayKeyedBroker) CancelOrder(_ context.Context, id string) (*port.CancelResult, error) {
	b.byPlainID = append(b.byPlainID, id)
	return nil, fmt.Errorf("tachibana: 営業日が引けず取消が外れた(order %s)", id)
}

// 板に残っている注文は GetActiveOrders にも出る(立花では一覧が同じ元データ)。
func (b *dayKeyedBroker) GetActiveOrders(context.Context, string) ([]order.Order, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	out := make([]order.Order, 0, len(b.resting))
	for _, o := range b.resting {
		out = append(out, order.Order{
			OrderID: o.OrderID, Symbol: o.Symbol, Side: o.Side,
			Quantity: o.Quantity, HasStopLeg: o.HasStopLeg,
		})
	}
	return out, nil
}

func (b *dayKeyedBroker) ClosePosition(context.Context, port.CloseRequest) (*port.CloseResult, error) {
	b.closeCalls++
	if b.rejectMsg != "" {
		return &port.CloseResult{Accepted: false}, errors.New(b.rejectMsg)
	}
	return &port.CloseResult{OrderID: "settle-1", Accepted: true, FilledPrice: 900}, nil
}

func dayKeyedFixture(t *testing.T, b *dayKeyedBroker) (*closeExecutor, position.Position, *safety.EmergencyStop) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 14, 51, 0, 0, clock.JST)
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	b.Paper.SetPrice("7203", 1000)
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "shinyo:7203", Symbol: "7203", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 1000, OpenedAt: now.Add(-72 * time.Hour),
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

// 🚨 前日以前に置いた守りは **営業日つきの経路で** 取り消さねばならない。
// CancelOrder(id) は営業日をプロセス内 map から引き、空なら "0"(当日)に落ちる。
// 守りは前のプロセスが出した注文なので map は空で、①取消が外れて拘束が解けず
// 返済が拒否される ②立花の注文番号は日付スコープなので "0" + 古い番号が
// **今日の別の注文**に当たりうる。②が最悪で、守りを残したまま無関係な注文を消す。
func TestCloseOne_CancelsProtectiveLegsWithTheBusinessDay(t *testing.T) {
	ctx := context.Background()
	b := &dayKeyedBroker{Paper: broker.NewPaper(clock.Fixed(time.Now()), 0, 0),
		resting: []port.ProtectiveOrderInfo{{
			OrderID: "board-sl", Symbol: "7203", Side: order.SideSell, Quantity: 100,
			HasStopLeg: true, BrokerRef: "20260818", // 建てた日の営業日
		}}}
	x, p, _ := dayKeyedFixture(t, b)

	if ok, err := x.closeOne(ctx, p, 1000, "max_hold", time.Now()); !ok || err != nil {
		t.Fatalf("決済は通るべき: ok=%v err=%v", ok, err)
	}
	if len(b.cancelled) != 1 || b.cancelled[0] != "board-sl" {
		t.Fatalf("板の守りを営業日つきで取り消していない: %v", b.cancelled)
	}
	// 🛑 台帳の古い leg id を撃っていないこと。これが「当日の別注文を掴む」経路。
	for _, id := range b.byPlainID {
		if id == "stale-tp" || id == "stale-sl" {
			t.Fatalf("台帳の古い leg id を盲撃ちしている(%s)— 立花の注文番号は日付スコープで、"+
				"当日扱いで撃つと今日の別の注文を取り消しうる", id)
		}
	}
}

// 🚨 板の照会が一過性で失敗したときに **建玉を CLOSING へ落としてはいけない**。
// CLOSING に落ちると誰も拾わない: ForceFlatten と ManageOpenPositions は StatusOpen
// 以外を skip し、reconcile の resolveStuckClosing は板に残った守りを「決済注文が
// 働いている」と読んで再送も escalate もしない。14:50 の強制フラット化が一過性の
// エラーで丸ごと止まり、建玉が置き去りになる。
//
// 建玉が OPEN のままなら次ティックが素直に再試行する。
func TestCloseOne_BoardQueryFailureLeavesThePositionOpenForRetry(t *testing.T) {
	ctx := context.Background()
	b := &dayKeyedBroker{Paper: broker.NewPaper(clock.Fixed(time.Now()), 0, 0),
		listErr: errors.New("CLMOrderList: session still inactive after re-login")}
	x, p, es := dayKeyedFixture(t, b)

	ok, err := x.closeOne(ctx, p, 1000, "max_hold", time.Now())
	if ok || err == nil {
		t.Fatalf("照会できないまま決済してはいけない: ok=%v err=%v", ok, err)
	}
	if b.closeCalls != 0 {
		t.Fatalf("板が見えないのに返済を撃っている(%d 回)— 守りを落とせたか分からないまま撃つと拘束で拒否される", b.closeCalls)
	}
	after, _ := x.posRepo.ListOpenOrClosing(ctx, "7203")
	if len(after) != 1 || after[0].Status != position.StatusOpen {
		t.Fatalf("建玉は OPEN のまま残さねばならない(次ティックで再試行する): %+v", after)
	}
	if es.Active() {
		t.Fatalf("一過性の照会エラーで実弾を緊急停止してはいけない: %q", es.Reason())
	}
}

// 取消が確認できないまま返済も拒否されたときは、**裸と断定しない**理由で鳴らす。
// 「本当に裸」と読んで守りを置き直すと、板に残っていた守りと二重になり、
// 返済可能数量を超えて**両方**弾かれうる。人間がまずやるべきは板を見ること。
func TestCloseOne_UnconfirmedCancelUsesADistinctTripReason(t *testing.T) {
	ctx := context.Background()
	b := &dayKeyedBroker{Paper: broker.NewPaper(clock.Fixed(time.Now()), 0, 0),
		resting: []port.ProtectiveOrderInfo{{
			OrderID: "board-sl", Symbol: "7203", Side: order.SideSell, Quantity: 100,
			HasStopLeg: true, BrokerRef: "", // 営業日を運べていない = 撃てない
		}},
		rejectMsg: "返済可能数量を超えています"}
	x, p, es := dayKeyedFixture(t, b)

	if ok, _ := x.closeOne(ctx, p, 1000, "max_hold", time.Now()); ok {
		t.Fatal("拒否された返済を成功として返してはいけない")
	}
	if !es.Active() {
		t.Fatal("守りを触った後に返済が拒否された = 人間を呼ばねばならない")
	}
	// Reason() は "<時刻> <理由>\n" 形式なので部分一致で見る。
	if got := es.Reason(); !strings.Contains(got, "close_rejected_cancel_unconfirmed:shinyo:7203") {
		t.Fatalf("取消を確認できていないのに「裸」と断定する理由で鳴っている: %q", got)
	}
}
