package command

import (
	"context"
	"errors"
	"fmt"
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

// boardFillStub は **板の守りが約定して broker から建玉が消えた**状態を作る。
// 建玉照会(GetPositions)が権威で、約定照会(GetExecutions)が実額の出どころ。
type boardFillStub struct {
	*broker.Paper
	hidePositions bool  // true = broker はこの建玉をもう持っていない
	positionsErr  error // 建玉照会そのものが落ちる(= 確かめられない)
	execs         []order.Execution
	execErr       error
	rejectClose   bool // ClosePosition が「信用建玉明細にデータがありません」で拒否される
}

func (b *boardFillStub) GetPositions(ctx context.Context) ([]port.BrokerPosition, error) {
	if b.positionsErr != nil {
		return nil, b.positionsErr
	}
	if b.hidePositions {
		return nil, nil
	}
	return b.Paper.GetPositions(ctx)
}

func (b *boardFillStub) GetExecutions(ctx context.Context, limit int) ([]order.Execution, error) {
	if b.execErr != nil {
		return nil, b.execErr
	}
	if b.execs != nil {
		return b.execs, nil
	}
	return b.Paper.GetExecutions(ctx, limit)
}

func (b *boardFillStub) ClosePosition(ctx context.Context, req port.CloseRequest) (*port.CloseResult, error) {
	if b.rejectClose {
		return nil, errors.New("tachibana: 信用建玉明細にデータがありません")
	}
	return b.Paper.ClosePosition(ctx, req)
}

func stoppedOutPosition(t *testing.T, ctx context.Context, posRepo port.PositionRepository, openedAt time.Time) position.Position {
	t.Helper()
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "shinyo:8604", Symbol: "8604", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 1670, StopLossPrice: 1596, HoldingMode: order.HoldingMultiday,
		ExecKind: order.ExecMarginSystem, Source: position.SourceBot, OpenedAt: openedAt,
	})
	if err != nil {
		t.Fatalf("建玉の作成: %v", err)
	}
	held, err := posRepo.ListOpenOrClosing(ctx, "8604")
	if err != nil || len(held) != 1 {
		t.Fatalf("建玉が読めない: %v %+v", err, held)
	}
	if held[0].ID != id {
		t.Fatalf("建玉 id がずれている: %d != %d", held[0].ID, id)
	}
	return held[0]
}

func onlyTrade(t *testing.T, ctx context.Context, trades *repository.InMemoryTradeRepo, since time.Time) port.TradeRecord {
	t.Helper()
	rows, err := trades.ListClosedSince(ctx, since)
	if err != nil {
		t.Fatalf("台帳が読めない: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("台帳の往復は 1 本のはず: %+v", rows)
	}
	return rows[0]
}

// 🚨 live で踏んだ穴。板の逆指値(SL 1,596)が約定して建玉が broker から
// 消えたのに、台帳には **1 時間余り後の時価 1,596.5** が `reconcile_cold_close`
// (画面表示「照合で消失」)として書かれた。
//
// 起きた出口は **損切り**で、値段は**実約定**。時価で埋めると戦績も edge-judge も
// 「起きていない出口」を数え、手数料も欠ける。建玉が broker から消えた理由の
// ほとんどは板の守りの約定 = 戦略の出口そのものなので、**まず約定照会に訊く**。
func TestReconcile_BooksTheBrokerSideStopFillNotTheObservedPrice(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 17, 10, 59, 46, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("8604", 1596.5) // 従来はこの観測値がそのまま台帳に載っていた
	brk := &boardFillStub{Paper: pb, hidePositions: true, execs: []order.Execution{
		{OrderID: "sl-1", Symbol: "8604", Side: order.SideSell, Quantity: 100, Price: 1596, FeeJPY: 275},
	}}
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	rec := NewReconcile(brk, posRepo, repository.NewCloser(posRepo, trades), safety.NewPendingPositions(), es, c)
	rec.gracePeriod = 0

	stoppedOutPosition(t, ctx, posRepo, now.Add(-10*24*time.Hour))

	rep, err := rec.Run(ctx, "8604")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.ColdClosed != 1 {
		t.Fatalf("消えた建玉は 1 本記帳されるはず: %+v", rep)
	}
	tr := onlyTrade(t, ctx, trades, now.Add(-time.Hour))
	if tr.ClosePrice != 1596 {
		t.Errorf("決済値 = %v、実約定 1596 であること(観測時価 1596.5 で埋めない)", tr.ClosePrice)
	}
	if tr.CloseReason != port.CloseReasonStopLoss {
		t.Errorf("決済理由 = %q、板の逆指値が約定したので %q であること(照合で消失ではない)",
			tr.CloseReason, port.CloseReasonStopLoss)
	}
	if tr.FeeJPY != 275 {
		t.Errorf("手数料 = %v、約定照会が返した実額 275 であること", tr.FeeJPY)
	}
	if tr.FeeEstimated {
		t.Error("実約定から取った手数料を estimated と書かない")
	}
	if es.Active() {
		t.Errorf("守りが仕事をしただけで緊急停止してはいけない: %q", es.Reason())
	}
}

// 🛑 **実約定が観測できないときは従来どおり**。時価の cold close は「確かめられない
// ものを書かない」の中で唯一許した近似なので、取り上げない(建玉が OPEN のまま
// 残ると枠と日次損失の計算が狂う)。
func TestReconcile_FallsBackToColdCloseWhenNoFillIsObservable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 17, 10, 59, 46, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("8604", 1596.5)
	brk := &boardFillStub{Paper: pb, hidePositions: true, execErr: errors.New("約定照会が落ちた")}
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	rec := NewReconcile(brk, posRepo, repository.NewCloser(posRepo, trades), safety.NewPendingPositions(), es, c)
	rec.gracePeriod = 0

	stoppedOutPosition(t, ctx, posRepo, now.Add(-10*24*time.Hour))

	if _, err := rec.Run(ctx, "8604"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	tr := onlyTrade(t, ctx, trades, now.Add(-time.Hour))
	if tr.CloseReason != "reconcile_cold_close" || tr.ClosePrice != 1596.5 {
		t.Fatalf("約定が観測できない枝は従来どおり時価の cold close: %+v", tr)
	}
}

// 🛑 **数量が合わない約定は採らない。** 約定照会は注文 id しか持たず「どの建玉の
// 決済か」を保証しないので、同じ銘柄に人間の売買が混ざった日に他人の値段で
// 台帳を締めうる。合わなければ従来の cold close に倒す(確かめられない値を書かない)。
func TestBoardSettleFill_RefusesWhenTheFilledQuantityDoesNotMatch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 17, 10, 59, 46, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	posRepo := repository.NewInMemoryPositionRepo()
	brk := &boardFillStub{Paper: pb, hidePositions: true, execs: []order.Execution{
		{OrderID: "sl-1", Symbol: "8604", Side: order.SideSell, Quantity: 100, Price: 1596},
		{OrderID: "human-1", Symbol: "8604", Side: order.SideSell, Quantity: 300, Price: 1601},
	}}
	p := stoppedOutPosition(t, ctx, posRepo, now.Add(-10*24*time.Hour))

	if _, _, _, ok := boardSettleFill(ctx, brk, p); ok {
		t.Fatal("建玉 100 株に対し売り約定が 400 株 — どれが自分の決済か決まらないので採ってはいけない")
	}
}

// 決済側の約定が建玉の SL / TP のどちら側で起きたかで理由が決まる。**側に依らない**
// (SELL 建玉では大小が逆になる)。どちらでもなければ人間か broker の都合による決済。
func TestBoardFillReason(t *testing.T) {
	buy := position.Position{Side: order.SideBuy, StopLossPrice: 1596, TakeProfitPrice: 1800}
	sell := position.Position{Side: order.SideSell, StopLossPrice: 1800, TakeProfitPrice: 1596}
	cases := []struct {
		name  string
		p     position.Position
		price float64
		want  string
	}{
		{"買い建玉が逆指値で約定", buy, 1596, port.CloseReasonStopLoss},
		{"買い建玉がギャップで逆指値を飛ばした", buy, 1500, port.CloseReasonStopLoss},
		{"買い建玉が利確指値で約定", buy, 1800, port.CloseReasonTakeProfit},
		{"買い建玉が帯の中で決済された", buy, 1700, port.CloseReasonBrokerClose},
		{"売り建玉が逆指値で約定", sell, 1800, port.CloseReasonStopLoss},
		{"売り建玉が利確指値で約定", sell, 1596, port.CloseReasonTakeProfit},
		{"売り建玉が帯の中で決済された", sell, 1700, port.CloseReasonBrokerClose},
	}
	for _, c := range cases {
		if got := boardFillReason(c.p, c.price); got != c.want {
			t.Errorf("%s: boardFillReason(%v) = %q, want %q", c.name, c.price, got, c.want)
		}
	}
}

// 🛑 **TP 脚が板に無い建玉(多日 = stop-only)に take_profit を出さない。**
// 凍結 TP は「戦略が決めた出口」であって板に載っているとは限らない
// (`ProtectiveTakeProfitOnBoard`)。値段だけで利確と読むと、実際には人間が締めた
// 決済を戦略の利確として台帳に書く。
func TestBoardFillReason_NoTakeProfitFrozenMeansNoTakeProfitLabel(t *testing.T) {
	trail := position.Position{Side: order.SideBuy, StopLossPrice: 1596} // TakeProfitPrice = 0
	if got := boardFillReason(trail, 1800); got != port.CloseReasonBrokerClose {
		t.Fatalf("凍結 TP の無い建玉の決済 = %q, want %q", got, port.CloseReasonBrokerClose)
	}
}

// 🚨 live で踏んだ穴そのもの。板の SL が先に約定して建玉が消えた
// ため返済が拒否され、`closeOne` はそれを **「取消は通った = 建玉は本当に裸」**と
// 読んで live を緊急停止した。**裸と決済済みは正反対**で、守るものが無い建玉に
// 守りは置けない。拒否の文面では判定せず、**建玉照会(唯一の権威)**で確かめる。
func TestCloseOne_RejectedBecauseTheBrokerNoLongerHoldsItBooksTheFillAndDoesNotTrip(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 17, 10, 59, 46, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("8604", 1596.5)
	brk := &boardFillStub{Paper: pb, hidePositions: true, rejectClose: true, execs: []order.Execution{
		{OrderID: "sl-1", Symbol: "8604", Side: order.SideSell, Quantity: 100, Price: 1596, FeeJPY: 275},
	}}
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	p := stoppedOutPosition(t, ctx, posRepo, now.Add(-10*24*time.Hour))

	exec := closeExecutor{broker: brk, posRepo: posRepo,
		closer: repository.NewCloser(posRepo, trades), emergency: es, unprotected: es}
	ok, err := exec.closeOne(ctx, p, 1596.5, port.CloseReasonStopLoss, now)
	if err != nil {
		t.Fatalf("既に決済済みの建玉は静かに台帳を締めるだけ: %v", err)
	}
	if !ok {
		t.Fatal("実約定が観測できたのだから台帳を締めること")
	}
	if es.Active() {
		t.Fatalf("broker に建玉が無い = 裸ではない。緊急停止してはいけない: %q", es.Reason())
	}
	tr := onlyTrade(t, ctx, trades, now.Add(-time.Hour))
	if tr.ClosePrice != 1596 || tr.CloseReason != port.CloseReasonStopLoss {
		t.Fatalf("板の逆指値の実約定で締めること: %+v", tr)
	}
}

// 🛑 **建玉が板に残っているなら従来どおり trip する。** 拒否の本来の意味(拘束 /
// 区分の取り違え)はそのままで、緩めたのは「建玉が無い」1 点だけ。
func TestCloseOne_RejectedWhileTheBrokerStillHoldsItStillTrips(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 17, 10, 59, 46, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("8604", 1596.5)
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	brk := &boardFillStub{Paper: pb, rejectClose: true}
	// broker は建玉を持ったまま(paper に実際に建てる)
	placed, err := pb.PlaceOrder(ctx, order.PlaceOrderRequest{Symbol: "8604", Side: order.SideBuy, Quantity: 100})
	if err != nil {
		t.Fatal(err)
	}
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: placed.BrokerPositionID, Symbol: "8604", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 1670, StopLossPrice: 1596, HoldingMode: order.HoldingMultiday,
		ExecKind: order.ExecMarginSystem, Source: position.SourceBot, OpenedAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	held, _ := posRepo.ListOpenOrClosing(ctx, "8604")
	if len(held) != 1 || held[0].ID != id {
		t.Fatalf("建玉が読めない: %+v", held)
	}

	exec := closeExecutor{broker: brk, posRepo: posRepo,
		closer: repository.NewCloser(posRepo, trades), emergency: es, unprotected: es}
	if ok, _ := exec.closeOne(ctx, held[0], 1596.5, port.CloseReasonStopLoss, now); ok {
		t.Fatal("拒否された返済で台帳を締めてはいけない")
	}
	if !es.Active() {
		t.Fatal("建玉が板に残ったまま返済が拒否された = 守りを cancel した後の裸。従来どおり trip すること")
	}
}

// 🛑 **建玉照会そのものが落ちたら従来どおり trip する(fail-close)。** 「確かめられ
// なかった」を「決済済み」と読むと、障害のたびに裸の建玉が静かに見逃される。
func TestCloseOne_RejectedAndPositionListingFailsStillTrips(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 17, 10, 59, 46, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("8604", 1596.5)
	brk := &boardFillStub{Paper: pb, rejectClose: true, positionsErr: errors.New("建玉照会が落ちた")}
	posRepo := repository.NewInMemoryPositionRepo()
	trades := repository.NewInMemoryTradeRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)
	p := stoppedOutPosition(t, ctx, posRepo, now.Add(-10*24*time.Hour))

	exec := closeExecutor{broker: brk, posRepo: posRepo,
		closer: repository.NewCloser(posRepo, trades), emergency: es, unprotected: es}
	if ok, _ := exec.closeOne(ctx, p, 1596.5, port.CloseReasonStopLoss, now); ok {
		t.Fatal("確かめられていないのに台帳を締めてはいけない")
	}
	if !es.Active() {
		t.Fatal("建玉照会が落ちた = 裸かどうか確かめられない。fail-close で trip すること")
	}
}

// 🚨 11:03〜12:18 の 15 分ごと、6 回。守りの自動復旧が **既に決済済みの建玉**に
// 逆指値を置こうとして毎回拒否され、ログには「この建玉はまだ裸」が並んだ。
// 裸の警報は本物のときだけ鳴らないと、鳴っても誰も見なくなる。
func TestRearmUnguarded_SkipsPositionsTheBrokerNoLongerHolds(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 17, 11, 3, 56, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("8604", 1596.5)
	brk := &boardFillStub{Paper: pb, hidePositions: true}
	posRepo := repository.NewInMemoryPositionRepo()
	p := stoppedOutPosition(t, ctx, posRepo, now.Add(-10*24*time.Hour))
	if ok, err := posRepo.ClaimForClose(ctx, p.ID, now); err != nil || !ok {
		t.Fatalf("CLOSING へ落とせない: %v %v", ok, err)
	}

	arm := NewArmProtectiveOrder(posRepo, brk, tokyoHours(), c)
	res, errs := NewRearmUnguarded(posRepo, brk, arm).Execute(ctx)
	if len(errs) != 0 {
		t.Fatalf("broker に建玉が無いのは「裸」ではない。error にしてはいけない: %v", errs)
	}
	if res.Armed != 0 {
		t.Fatalf("決済済みの建玉に守りを置こうとしてはいけない: %+v", res)
	}
	if res.Settled != 1 {
		t.Fatalf("broker に建玉が無い本数を別に数えること(reconcile が台帳を締める): %+v", res)
	}
}

// 🛑 **建玉照会が落ちたら従来どおり置きに行く。** この経路はリスクを単調に減らす
// (守りを置くだけ)ので、確かめられないときに黙るほうが危ない。ここでは置きに
// 行った先で拒否されるので、**skip されず error として上がる**ことで「行った」を見る。
func TestRearmUnguarded_DoesNotGoSilentWhenThePositionListingFails(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 17, 11, 3, 56, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("8604", 1596.5)
	brk := &boardFillStub{Paper: pb, positionsErr: fmt.Errorf("建玉照会が落ちた")}
	posRepo := repository.NewInMemoryPositionRepo()
	stoppedOutPosition(t, ctx, posRepo, now.Add(-10*24*time.Hour))

	arm := NewArmProtectiveOrder(posRepo, brk, tokyoHours(), c)
	res, errs := NewRearmUnguarded(posRepo, brk, arm).Execute(ctx)
	if res.Settled != 0 {
		t.Fatalf("確かめられていないのに「決済済み」と数えてはいけない: %+v", res)
	}
	if len(errs) != 1 {
		t.Fatalf("置きに行った結果は黙らずに報せること: %v", errs)
	}
}
