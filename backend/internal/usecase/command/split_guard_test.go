package command

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// 1:5 分割の権利落ち日(2026-09-29 の 6368)を再現する材料。
var (
	splitExDate  = time.Date(2026, 9, 29, 9, 0, 5, 0, clock.JST)
	splitOpened  = time.Date(2026, 9, 16, 9, 3, 8, 0, clock.JST)
	splitPrevDay = time.Date(2026, 9, 28, 0, 0, 0, 0, clock.JST)
)

type splitFixture struct {
	repo    *repository.InMemoryPositionRepo
	candles *repository.InMemoryCandleRepo
	paper   *broker.Paper
	trip    *fakeTripper
	id      int64
}

func newSplitFixture(t *testing.T, prevBarDay time.Time, prevClose float64) *splitFixture {
	t.Helper()
	ctx := context.Background()
	f := &splitFixture{
		repo:    repository.NewInMemoryPositionRepo(),
		candles: repository.NewInMemoryCandleRepo(),
		paper:   broker.NewPaper(func() time.Time { return splitExDate }, 0, 0),
		trip:    &fakeTripper{},
	}
	if err := f.candles.Upsert(ctx, "6368", []market.Candle{
		{OpenTime: prevBarDay.AddDate(0, 0, -1), Interval: 24 * time.Hour, Open: 12690, High: 12700, Low: 12470, Close: 12690, Volume: 1},
		{OpenTime: prevBarDay, Interval: 24 * time.Hour, Open: 12880, High: 12895, Low: 12570, Close: prevClose, Volume: 1},
	}); err != nil {
		t.Fatal(err)
	}
	id, err := f.repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "pos-24", Symbol: "6368", Side: order.SideBuy, Quantity: 100, EntryPrice: 11570,
		TakeProfitJPY: 1900, TakeProfitPrice: 13470, StopLossJPY: 1000, StopLossPrice: 10570,
		MaxHoldMinutes: 30 * 24 * 60, HoldingMode: order.HoldingMultiday, Source: position.SourceBot,
		StrategyName: "bnf_day2_reversion", OpenedAt: splitOpened,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.id = id
	f.paper.AdoptOpenPositions([]port.BrokerPosition{{BrokerPositionID: "pos-24", Symbol: "6368", Side: order.SideBuy, Quantity: 100, EntryPrice: 11570}})
	return f
}

func (f *splitFixture) manage(brk port.Broker, g *SplitGuard) *ManageOpenPositions {
	return NewManageOpenPositions(brk, f.repo, repository.NewCloser(f.repo, repository.NewInMemoryTradeRepo()),
		f.trip, func() time.Time { return splitExDate }, 0).WithSplitGuard(g)
}

func (f *splitFixture) get(t *testing.T) position.Position {
	t.Helper()
	p, err := f.repo.GetByID(context.Background(), f.id)
	if err != nil || p == nil {
		t.Fatalf("GetByID: %v %v", p, err)
	}
	return *p
}

// paper: 権利落ちの寄り(2,495 円)で損切りせず、500 株・建値 2,314 円へ言い直す。紙の帳簿も揃える。
func TestSplitGuard_PaperAdjustsInsteadOfStoppingOut(t *testing.T) {
	ctx := context.Background()
	f := newSplitFixture(t, splitPrevDay, 12595)
	f.paper.SetPrice("6368", 2495)
	g := NewSplitGuard(f.repo, f.candles, f.paper, testHours(), f.trip, SplitAuthorityPrice).WithPaperBook(f.paper)
	m := f.manage(f.paper, g)

	if err := m.OnTick(ctx, "6368", summaryAt("6368", 2495, splitExDate)); err != nil {
		t.Fatal(err)
	}
	p := f.get(t)
	if p.Status != position.StatusOpen {
		t.Fatalf("分割を損切りと読んで決済した(status %s)", p.Status)
	}
	if p.Quantity != 500 || p.EntryPrice != 2314 || p.StopLossPrice != 2114 || p.SplitFactor != 5 {
		t.Fatalf("言い直しが違う: qty %d entry %v sl %v factor %v", p.Quantity, p.EntryPrice, p.StopLossPrice, p.SplitFactor)
	}
	bps, _ := f.paper.GetPositions(ctx)
	if len(bps) != 1 || bps[0].Quantity != 500 || bps[0].EntryPrice != 2314 {
		t.Fatalf("紙の帳簿が揃っていない: %+v", bps)
	}
	if len(f.trip.reasons) != 0 {
		t.Fatalf("paper の分割調整で緊急停止した: %v", f.trip.reasons)
	}

	// 同じ日の次のティック(と再起動後)では二度割らない。
	if err := m.OnTick(ctx, "6368", summaryAt("6368", 2500, splitExDate.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if p := f.get(t); p.Quantity != 500 {
		t.Fatalf("同じ権利落ち日に二度割った: qty %d", p.Quantity)
	}

	// 調整後は分割後の値段で SL が効く(分割前 10,570 = 分割後 2,114)。
	if err := m.OnTick(ctx, "6368", summaryAt("6368", 2100, splitExDate.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if p := f.get(t); p.Status == position.StatusOpen {
		t.Fatal("調整後の SL(2,114 円)を割っても決済しない")
	}
}

// 権利落ち日に**建てた**建玉は最初から分割後の値段なので触らない。
func TestSplitGuard_IgnoresPositionOpenedOnExDate(t *testing.T) {
	ctx := context.Background()
	f := newSplitFixture(t, splitPrevDay, 12595)
	id, _ := f.repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "pos-99", Symbol: "6368", Side: order.SideBuy, Quantity: 100, EntryPrice: 2480,
		StopLossJPY: 200, StopLossPrice: 2280, HoldingMode: order.HoldingMultiday, Source: position.SourceBot,
		OpenedAt: splitExDate.Add(-time.Second),
	})
	g := NewSplitGuard(f.repo, f.candles, f.paper, testHours(), f.trip, SplitAuthorityPrice).WithPaperBook(f.paper)
	if err := f.manage(f.paper, g).OnTick(ctx, "6368", summaryAt("6368", 2495, splitExDate)); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.repo.GetByID(ctx, id); p.Quantity != 100 || p.EntryPrice != 2480 {
		t.Fatalf("当日建ての建玉を分割調整した: %+v", p)
	}
}

// 🛑 値幅制限の内側の暴落は分割ではない — 今までどおり SL で決済する。
func TestSplitGuard_RealCrashStillStopsOut(t *testing.T) {
	ctx := context.Background()
	f := newSplitFixture(t, splitPrevDay, 12595)
	g := NewSplitGuard(f.repo, f.candles, f.paper, testHours(), f.trip, SplitAuthorityPrice).WithPaperBook(f.paper)
	f.paper.SetPrice("6368", 10000)
	if err := f.manage(f.paper, g).OnTick(ctx, "6368", summaryAt("6368", 10000, splitExDate)); err != nil {
		t.Fatal(err)
	}
	if p := f.get(t); p.Status == position.StatusOpen || p.Quantity != 100 {
		t.Fatalf("帯の内側の急落で SL が効かない: %+v", p)
	}
}

// 🛑 前日終値が**前営業日のもの**でなければ判定しない(2 日ぶんの値動きを 1 日の帯で測ると、
// 実相場の連続ストップ安を分割と読みうる)。古い日足は従来どおりの扱い。
func TestSplitGuard_StalePrevCloseDoesNotDetect(t *testing.T) {
	ctx := context.Background()
	f := newSplitFixture(t, splitPrevDay.AddDate(0, 0, -4), 12595) // 9/24 のバーが最新(9/25・9/28 が欠け)
	g := NewSplitGuard(f.repo, f.candles, f.paper, testHours(), f.trip, SplitAuthorityPrice).WithPaperBook(f.paper)
	out, hold := g.Screen(ctx, "6368", 2495, splitExDate, []position.Position{f.get(t)})
	if len(hold) != 0 || out[0].Quantity != 100 {
		t.Fatalf("古い前日終値で分割を断定した: hold %v qty %d", hold, out[0].Quantity)
	}
}

// liveBrokerStub は建玉照会だけを持つ live broker の代役。
type liveBrokerStub struct {
	port.Broker
	positions []port.BrokerPosition
	err       error
}

func (b *liveBrokerStub) GetPositions(context.Context) ([]port.BrokerPosition, error) {
	return b.positions, b.err
}

// live: broker の建玉照会(唯一の権威)が 500 株を示したときだけ台帳を言い直す。
func TestSplitGuard_LiveAdjustsWhenBrokerConfirms(t *testing.T) {
	ctx := context.Background()
	f := newSplitFixture(t, splitPrevDay, 12595)
	brk := &liveBrokerStub{positions: []port.BrokerPosition{{BrokerPositionID: "pos-24", Symbol: "6368", Side: order.SideBuy, Quantity: 500, EntryPrice: 2314}}}
	g := NewSplitGuard(f.repo, f.candles, brk, testHours(), f.trip, SplitAuthorityBroker)
	out, hold := g.Screen(ctx, "6368", 2495, splitExDate, []position.Position{f.get(t)})
	if len(hold) != 0 {
		t.Fatalf("broker が確認したのに決済を止めた: %v", hold)
	}
	if out[0].Quantity != 500 || f.get(t).Quantity != 500 {
		t.Fatalf("台帳を言い直していない: out %d ledger %d", out[0].Quantity, f.get(t).Quantity)
	}
	if len(f.trip.reasons) != 0 {
		t.Fatalf("確認できたのに緊急停止した: %v", f.trip.reasons)
	}
}

// 🛑 live: broker がまだ 100 株(未調整)なら**台帳を推測で書き換えない**。その建玉の自動決済を
// 止め、緊急停止して人間に回す(分割前の SL で 100 株だけ売ると、残りが守りの無い建玉になる)。
func TestSplitGuard_LiveHoldsAndTripsWhenBrokerDisagrees(t *testing.T) {
	ctx := context.Background()
	f := newSplitFixture(t, splitPrevDay, 12595)
	brk := &liveBrokerStub{positions: []port.BrokerPosition{{BrokerPositionID: "pos-24", Symbol: "6368", Side: order.SideBuy, Quantity: 100, EntryPrice: 11570}}}
	g := NewSplitGuard(f.repo, f.candles, brk, testHours(), f.trip, SplitAuthorityBroker)
	m := f.manage(brk, g)
	if err := m.OnTick(ctx, "6368", summaryAt("6368", 2495, splitExDate)); err != nil {
		t.Fatal(err)
	}
	p := f.get(t)
	if p.Status != position.StatusOpen || p.Quantity != 100 {
		t.Fatalf("確認できないまま決済 / 書き換えた: %+v", p)
	}
	if len(f.trip.reasons) != 1 || !strings.HasPrefix(f.trip.reasons[0], "split_unconfirmed:6368") {
		t.Fatalf("緊急停止していない / 理由が違う: %v", f.trip.reasons)
	}
	// 次のティックでも決済しない・緊急停止を重ねない。
	if err := m.OnTick(ctx, "6368", summaryAt("6368", 2400, splitExDate.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if p := f.get(t); p.Status != position.StatusOpen {
		t.Fatal("保留中の建玉を 2 ティック目で決済した")
	}
	if len(f.trip.reasons) != 1 {
		t.Fatalf("緊急停止を重ねた: %v", f.trip.reasons)
	}
}

// 照会が落ちた回は確かめられていない = 保留(fail-close)。
func TestSplitGuard_LiveHoldsWhenBrokerQueryFails(t *testing.T) {
	ctx := context.Background()
	f := newSplitFixture(t, splitPrevDay, 12595)
	brk := &liveBrokerStub{err: errors.New("timeout")}
	g := NewSplitGuard(f.repo, f.candles, brk, testHours(), f.trip, SplitAuthorityBroker)
	_, hold := g.Screen(ctx, "6368", 2495, splitExDate, []position.Position{f.get(t)})
	if !hold[f.id] {
		t.Fatal("照会できないのに決済判定を続けた")
	}
	if len(f.trip.reasons) != 1 {
		t.Fatalf("緊急停止していない: %v", f.trip.reasons)
	}
}

// 株数だけが分割後に見えても、建単価が分割前のままなら言い直さない(broker 側の反映途中・別の操作)。
func TestSplitGuard_LiveHoldsWhenBrokerEntryPriceDisagrees(t *testing.T) {
	ctx := context.Background()
	f := newSplitFixture(t, splitPrevDay, 12595)
	brk := &liveBrokerStub{positions: []port.BrokerPosition{{BrokerPositionID: "pos-24", Symbol: "6368", Side: order.SideBuy, Quantity: 500, EntryPrice: 11570}}}
	g := NewSplitGuard(f.repo, f.candles, brk, testHours(), f.trip, SplitAuthorityBroker)
	_, hold := g.Screen(ctx, "6368", 2495, splitExDate, []position.Position{f.get(t)})
	if !hold[f.id] || f.get(t).Quantity != 100 {
		t.Fatalf("建単価が合わないのに言い直した: hold %v qty %d", hold, f.get(t).Quantity)
	}
}

// paper で言い直せない建玉(株数が端数になる)は、その日は自動決済しないが**緊急停止はしない**
// (研究トラックの新規を丸ごと止める理由にならない)。
func TestSplitGuard_PaperUnadjustableHoldsWithoutTripping(t *testing.T) {
	ctx := context.Background()
	f := newSplitFixture(t, splitPrevDay, 1200)
	p := f.get(t)
	p.Quantity = 1
	g := NewSplitGuard(f.repo, f.candles, f.paper, testHours(), f.trip, SplitAuthorityPrice)
	_, hold := g.Screen(ctx, "6368", 800, splitExDate, []position.Position{p}) // 1:1.5 → 1.5 株
	if !hold[p.ID] {
		t.Fatal("言い直せない建玉を分割前の凍結値で決済判定に回した")
	}
	if len(f.trip.reasons) != 0 {
		t.Fatalf("paper で緊急停止した: %v", f.trip.reasons)
	}
}

// 🚨 1:1.2 のような小さい分割は値幅制限の内側(−17%)に収まり、値段からは断定できない。
// live は broker の建玉照会(株数 ×r・建単価 ÷r)が唯一の権威なので、**その日の最初のティックで
// 値段に依らず 1 度照会**し、分割後を示していれば言い直す(分割前の SL で誤決済しない)。
func TestSplitGuard_LiveDetectsSmallSplitFromBrokerWithoutPriceEvidence(t *testing.T) {
	ctx := context.Background()
	f := newSplitFixture(t, splitPrevDay, 12000)
	brk := &liveBrokerStub{positions: []port.BrokerPosition{{BrokerPositionID: "pos-24", Symbol: "6368", Side: order.SideBuy, Quantity: 120, EntryPrice: 11570 / 1.2}}}
	g := NewSplitGuard(f.repo, f.candles, brk, testHours(), f.trip, SplitAuthorityBroker)
	m := f.manage(brk, g)
	// 権利落ちの寄り 10,000(= 12,000 ÷ 1.2)は帯の内側。分割前の SL 10,570 は「割って」いる。
	if err := m.OnTick(ctx, "6368", summaryAt("6368", 10000, splitExDate)); err != nil {
		t.Fatal(err)
	}
	p := f.get(t)
	if p.Status != position.StatusOpen {
		t.Fatalf("1:1.2 分割を損切りと読んで決済した(status %s)", p.Status)
	}
	if p.Quantity != 120 || math.Abs(p.SplitFactor-1.2) > 1e-9 {
		t.Fatalf("broker の建玉照会どおりに言い直していない: qty %d factor %v", p.Quantity, p.SplitFactor)
	}
	if len(f.trip.reasons) != 0 {
		t.Fatalf("確かめられたのに緊急停止した: %v", f.trip.reasons)
	}
}

// broker の照会は**その日 1 回**(銘柄ごと)。毎ティック叩かない(通信量の不変条件)。
func TestSplitGuard_LiveBrokerCheckOncePerDay(t *testing.T) {
	ctx := context.Background()
	f := newSplitFixture(t, splitPrevDay, 11600)
	brk := &countingBrokerStub{liveBrokerStub: liveBrokerStub{positions: []port.BrokerPosition{{BrokerPositionID: "pos-24", Symbol: "6368", Side: order.SideBuy, Quantity: 100, EntryPrice: 11570}}}}
	g := NewSplitGuard(f.repo, f.candles, brk, testHours(), f.trip, SplitAuthorityBroker)
	for i := 0; i < 5; i++ {
		g.Screen(ctx, "6368", 11600, splitExDate.Add(time.Duration(i)*time.Minute), []position.Position{f.get(t)})
	}
	if brk.calls != 1 {
		t.Fatalf("建玉照会 %d 回 — その日 1 回のはず", brk.calls)
	}
	if p := f.get(t); p.Quantity != 100 {
		t.Fatalf("分割でないのに言い直した: qty %d", p.Quantity)
	}
}

type countingBrokerStub struct {
	liveBrokerStub
	calls int
}

func (b *countingBrokerStub) GetPositions(ctx context.Context) ([]port.BrokerPosition, error) {
	b.calls++
	return b.liveBrokerStub.GetPositions(ctx)
}

// 立花の公式(株式分割等の場合における信用建玉の取扱い): **整数倍以外の分割(1:1.2 等)は株数を
// 増やさず、建単価から権利処理価格を差し引く**。株数が同じまま建単価だけが下がった建玉は
// 分割前の凍結値で決済判定してはいけない(誤損切り)。比を確定できないので言い直さず、
// その日は自動決済を止めて緊急停止する(人間が broker の画面で確かめる)。
func TestSplitGuard_LiveHoldsNonIntegerSplitWhereBrokerOnlyCutsEntryPrice(t *testing.T) {
	ctx := context.Background()
	f := newSplitFixture(t, splitPrevDay, 12000)
	brk := &liveBrokerStub{positions: []port.BrokerPosition{{BrokerPositionID: "pos-24", Symbol: "6368", Side: order.SideBuy, Quantity: 100, EntryPrice: 11570 - 2000}}}
	g := NewSplitGuard(f.repo, f.candles, brk, testHours(), f.trip, SplitAuthorityBroker)
	if err := f.manage(brk, g).OnTick(ctx, "6368", summaryAt("6368", 10000, splitExDate)); err != nil {
		t.Fatal(err)
	}
	if p := f.get(t); p.Status != position.StatusOpen || p.Quantity != 100 || p.EntryPrice != 11570 {
		t.Fatalf("整数倍以外の分割で決済 / 推測で言い直した: %+v", p)
	}
	if len(f.trip.reasons) != 1 || !strings.HasPrefix(f.trip.reasons[0], "split_unconfirmed:6368") {
		t.Fatalf("緊急停止していない: %v", f.trip.reasons)
	}
}
