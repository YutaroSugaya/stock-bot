package command

import (
	"context"
	"errors"
	"fmt"
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
	"stockbot/backend/internal/safety"
	"stockbot/backend/internal/testutil"
)

type closeHarness struct {
	broker    port.Broker
	paper     *broker.Paper
	posRepo   *repository.InMemoryPositionRepo
	tradeRepo *repository.InMemoryTradeRepo
	emergency *safety.EmergencyStop
	cmd       *CloseAllOpen
	tradeable map[string]*market.MarketSummary
	indic     map[string]*market.MarketSummary
	logs      []string
	now       time.Time
}

func newCloseHarness(t *testing.T, now time.Time, brk func(*broker.Paper) port.Broker) *closeHarness {
	t.Helper()
	return newCloseHarnessAlarm(t, now, brk, true)
}

// newCloseHarnessWithoutAlarm は research / harvest(紙)と同じ「警報なし」構成。
func newCloseHarnessWithoutAlarm(t *testing.T, now time.Time, brk func(*broker.Paper) port.Broker) *closeHarness {
	t.Helper()
	return newCloseHarnessAlarm(t, now, brk, false)
}

func newCloseHarnessAlarm(t *testing.T, now time.Time, brk func(*broker.Paper) port.Broker, alarm bool) *closeHarness {
	t.Helper()
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 0, 0)
	h := &closeHarness{
		paper:     pb,
		posRepo:   repository.NewInMemoryPositionRepo(),
		tradeRepo: repository.NewInMemoryTradeRepo(),
		emergency: safety.NewEmergencyStop(testutil.TempFlagPath(t), nil),
		tradeable: map[string]*market.MarketSummary{},
		indic:     map[string]*market.MarketSummary{},
		now:       now,
	}
	h.broker = port.Broker(pb)
	if brk != nil {
		h.broker = brk(pb)
	}
	closer := repository.NewCloser(h.posRepo, h.tradeRepo)
	// 🛑 unprotected に **h.emergency を渡す**。これで
	// TestCloseAllOpen_RejectedCloseDoesNotTripEmergency が
	// 「拒否では鳴らない」を**警報が配線された状態で**証明する回帰ガードになる
	// (nil を渡すと、鳴らないのが分離のおかげか未配線のせいか区別できない)。
	h.cmd = NewCloseAllOpen(h.broker, h.posRepo, closer, func(symbol string) (*market.MarketSummary, *market.MarketSummary) {
		return h.tradeable[symbol], h.indic[symbol]
	}, c, unprotectedFor(h, alarm)).WithLogger(func(msg string, args ...any) {
		h.logs = append(h.logs, msg+" "+fmt.Sprintln(args...))
	})
	return h
}

func (h *closeHarness) open(t *testing.T, symbol string, price float64, src position.Source, mode order.HoldingMode, ek order.ExecKind) int64 {
	t.Helper()
	ctx := context.Background()
	h.paper.SetPrice(symbol, price)
	placed, err := h.paper.PlaceOrder(ctx, order.PlaceOrderRequest{Symbol: symbol, Side: order.SideBuy, Quantity: 100})
	if err != nil {
		t.Fatalf("place %s: %v", symbol, err)
	}
	id, err := h.posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: placed.BrokerPositionID, Symbol: symbol, Side: order.SideBuy,
		Quantity: 100, EntryPrice: price, OpenedAt: h.now, Source: src,
		HoldingMode: mode, ExecKind: ek,
	})
	if err != nil {
		t.Fatalf("insert %s: %v", symbol, err)
	}
	h.tradeable[symbol] = market.SummaryFromTicker(market.Ticker{Symbol: symbol, Bid: price, Ask: price, Last: price}, h.now)
	return id
}

func closeAllNow() time.Time { return time.Date(2026, 8, 7, 15, 20, 0, 0, clock.JST) }

// HoldingMode を問わず全部閉じ、close_reason=manual で trades に載る。
func TestCloseAllOpen_ClosesEveryBotPositionAsManual(t *testing.T) {
	ctx := context.Background()
	now := closeAllNow()
	h := newCloseHarness(t, now, nil)
	h.open(t, "7203", 2500, position.SourceBot, order.HoldingMultiday, order.ExecMarginGeneral)
	h.open(t, "6758", 3000, position.SourceBot, order.HoldingIntraday, order.ExecMarginOneday)

	res, err := h.cmd.Execute(ctx)
	if err != nil {
		t.Fatalf("close-all: %v", err)
	}
	if res.Closed != 2 || res.Failed != 0 || res.SkippedExternal != 0 {
		t.Fatalf("want closed=2 failed=0 skipped=0, got %+v", res)
	}
	open, _ := h.posRepo.ListOpenAllSymbols(ctx)
	if len(open) != 0 {
		t.Fatalf("all bot positions must be closed, still open: %+v", open)
	}
	trades, _ := h.tradeRepo.ListClosedSince(ctx, now.Add(-time.Hour))
	if len(trades) != 2 {
		t.Fatalf("expected 2 recorded trades, got %d", len(trades))
	}
	for _, tr := range trades {
		if tr.CloseReason != "manual" {
			t.Fatalf("close_reason must be manual (裁量の打ち切りと機械的に区別する), got %q", tr.CloseReason)
		}
	}
}

func TestCloseAllOpen_SkipsExternalPositions(t *testing.T) {
	ctx := context.Background()
	now := closeAllNow()
	h := newCloseHarness(t, now, nil)
	h.open(t, "7203", 2500, position.SourceBot, order.HoldingMultiday, order.ExecMarginGeneral)
	extID := h.open(t, "9984", 8000, position.SourceExternal, order.HoldingMultiday, order.ExecMarginGeneral)

	res, err := h.cmd.Execute(ctx)
	if err != nil {
		t.Fatalf("close-all: %v", err)
	}
	if res.Closed != 1 || res.SkippedExternal != 1 {
		t.Fatalf("want closed=1 skipped_external=1, got %+v", res)
	}
	open, _ := h.posRepo.ListOpenAllSymbols(ctx)
	if len(open) != 1 || open[0].ID != extID {
		t.Fatalf("external position must remain untouched, got %+v", open)
	}
}

// 値が付かない銘柄は indicative(前日終値)に落として締める。indicative 評価は
// 帳簿締め専用の例外なので、どちらを使ったかは必ずログに残す。
func TestCloseAllOpen_FallsBackToIndicativeQuote(t *testing.T) {
	ctx := context.Background()
	now := closeAllNow()
	h := newCloseHarness(t, now, nil)
	h.open(t, "6976", 1200, position.SourceBot, order.HoldingMultiday, order.ExecMarginGeneral)
	delete(h.tradeable, "6976") // 約定した値段は一度も立っていない
	h.indic["6976"] = market.SummaryFromTicker(market.Ticker{Symbol: "6976", Last: 1180, Stale: true}, now)

	if _, err := h.cmd.Execute(ctx); err != nil {
		t.Fatalf("close-all: %v", err)
	}
	joined := strings.Join(h.logs, "\n")
	if !strings.Contains(joined, "indicative") {
		t.Fatalf("indicative フォールバックがログに残っていない: %q", joined)
	}
}

// 帳簿締めで bot を止めないので、close 拒否でも trip しない。建玉は CLOSING の
// まま reconcile に任せる。
func TestCloseAllOpen_RejectedCloseDoesNotTripEmergency(t *testing.T) {
	ctx := context.Background()
	now := closeAllNow()
	h := newCloseHarness(t, now, func(p *broker.Paper) port.Broker { return rejectingCloseBroker{Paper: p} })
	h.open(t, "7203", 2500, position.SourceBot, order.HoldingMultiday, order.ExecMarginGeneral)
	h.open(t, "6758", 3000, position.SourceBot, order.HoldingMultiday, order.ExecMarginGeneral)

	res, err := h.cmd.Execute(ctx)
	if err != nil {
		t.Fatalf("close-all must not fail the whole run: %v", err)
	}
	if res.Failed != 2 || res.Closed != 0 {
		t.Fatalf("want failed=2 closed=0 (失敗しても続行), got %+v", res)
	}
	if h.emergency.Active() {
		t.Fatalf("帳簿締めの close 失敗で emergency を trip してはいけない: %q", h.emergency.Reason())
	}
}

func TestCloseOne_ClosesOnlyThatPosition(t *testing.T) {
	ctx := context.Background()
	now := closeAllNow()
	h := newCloseHarness(t, now, nil)
	id := h.open(t, "7203", 2500, position.SourceBot, order.HoldingMultiday, order.ExecMarginGeneral)
	other := h.open(t, "6758", 3000, position.SourceBot, order.HoldingMultiday, order.ExecMarginGeneral)

	res, err := h.cmd.CloseOne(ctx, id)
	if err != nil {
		t.Fatalf("close one: %v", err)
	}
	if res == nil || res.PositionID != id || res.Symbol != "7203" || res.Quantity != 100 || !res.Closed {
		t.Fatalf("unexpected result: %+v", res)
	}
	open, _ := h.posRepo.ListOpenAllSymbols(ctx)
	if len(open) != 1 || open[0].ID != other {
		t.Fatalf("only the requested position may close, got %+v", open)
	}
	trades, _ := h.tradeRepo.ListClosedSince(ctx, now.Add(-time.Hour))
	if len(trades) != 1 || trades[0].CloseReason != "manual" {
		t.Fatalf("expected 1 manual trade, got %+v", trades)
	}
}

func TestCloseOne_UnknownIDAndNotOpenAndExternal(t *testing.T) {
	ctx := context.Background()
	now := closeAllNow()
	h := newCloseHarness(t, now, nil)
	id := h.open(t, "7203", 2500, position.SourceBot, order.HoldingMultiday, order.ExecMarginGeneral)
	ext := h.open(t, "9984", 8000, position.SourceExternal, order.HoldingMultiday, order.ExecMarginGeneral)

	if _, err := h.cmd.CloseOne(ctx, 9999); !errors.Is(err, ErrPositionNotFound) {
		t.Fatalf("unknown id must be ErrPositionNotFound, got %v", err)
	}
	if _, err := h.cmd.CloseOne(ctx, ext); !errors.Is(err, ErrPositionExternal) {
		t.Fatalf("external must be ErrPositionExternal, got %v", err)
	}
	if _, err := h.cmd.CloseOne(ctx, id); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// 二度目(既に CLOSED)は「見つからない」ではなく「OPEN でない」。
	if _, err := h.cmd.CloseOne(ctx, id); !errors.Is(err, ErrPositionNotOpen) {
		t.Fatalf("closed position must be ErrPositionNotOpen, got %v", err)
	}
}

// 決済はリスクを減らす方向なので emergency 中でも通す —「手動 override は hard
// gate をバイパスしない」は entry の規律。
func TestCloseOne_WorksDuringEmergency(t *testing.T) {
	ctx := context.Background()
	now := closeAllNow()
	h := newCloseHarness(t, now, nil)
	id := h.open(t, "7203", 2500, position.SourceBot, order.HoldingMultiday, order.ExecMarginGeneral)
	if err := h.emergency.Trip("test", now); err != nil {
		t.Fatal(err)
	}

	if _, err := h.cmd.CloseOne(ctx, id); err != nil {
		t.Fatalf("emergency 中でも決済は通るべき: %v", err)
	}
	open, _ := h.posRepo.ListOpenAllSymbols(ctx)
	if len(open) != 0 {
		t.Fatalf("position should be closed, got %+v", open)
	}
}

// rejectingCloseBroker rejects every settle order (立花が拘束などで蹴る状況)。
type rejectingCloseBroker struct{ *broker.Paper }

func (b rejectingCloseBroker) ClosePosition(_ context.Context, _ port.CloseRequest) (*port.CloseResult, error) {
	return &port.CloseResult{Accepted: false, Message: "rejected"}, nil
}

// unprotectedFor は「裸の建玉だけを拾う警報」を配線するかどうかを切り替える。
// 🛑 interface へ入れるので **nil を型付きで返さない**(型付き nil は != nil になる)。
func unprotectedFor(h *closeHarness, alarm bool) EmergencyController {
	if !alarm {
		return nil
	}
	return h.emergency
}
