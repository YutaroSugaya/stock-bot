package command

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/port"
)

// 🛑 「ラベルの嘘を直す」は **配線 1 行**(`closeExecutor.closeOne` が
// `port.RatchetCloseReason` を通す)に乗っている。純関数側のテストは充実していても、
// **その 1 行が剥がれたら全テストが緑のまま `ratchet_takeprofit` が損失で出る**状態に戻る
// (監査で指摘)。台帳に実際に書かれる文字列で縛る。
func TestCloseOneLabelsALosingRatchetExitAsGivebackLoss(t *testing.T) {
	ctx := context.Background()
	f := newSaga(t, nil)

	// 建玉を作る(paper broker は即約定)。
	posID, err := f.enter(ctx, 100)
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	p, err := f.posRepo.GetByID(ctx, posID)
	if err != nil || p == nil {
		t.Fatalf("建玉が読めない: %v", err)
	}

	// 建値より **下** で ratchet 決済する = gross < 0。
	loss := p.EntryPrice - 10
	f.paper.SetPrice("7203", loss)
	x := closeExecutor{
		broker: f.paper, posRepo: f.posRepo,
		closer: repository.NewCloser(f.posRepo, f.trades), emergency: f.es,
	}
	ok, err := x.closeOne(ctx, *p, loss, port.CloseReasonRatchetTakeProfit, f.now)
	if err != nil || !ok {
		t.Fatalf("closeOne: ok=%v err=%v", ok, err)
	}

	trs, _ := f.trades.ListClosedSince(ctx, f.now.Add(-time.Hour))
	if len(trs) != 1 {
		t.Fatalf("trade 行 = %d, want 1", len(trs))
	}
	if trs[0].ProfitLossJPY >= 0 {
		t.Fatalf("前提: gross は負であること: %v", trs[0].ProfitLossJPY)
	}
	if trs[0].CloseReason != port.CloseReasonRatchetGivebackLoss {
		t.Fatalf("close_reason = %q, want %q — 損失なのに『利確』として台帳に残る(嘘が戻っている)",
			trs[0].CloseReason, port.CloseReasonRatchetGivebackLoss)
	}
}

// 利益で出たトレール決済は従来どおり `ratchet_takeprofit`。
func TestCloseOneKeepsTakeProfitLabelWhenGrossIsPositive(t *testing.T) {
	ctx := context.Background()
	f := newSaga(t, nil)
	posID, err := f.enter(ctx, 100)
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	p, _ := f.posRepo.GetByID(ctx, posID)
	win := p.EntryPrice + 10
	f.paper.SetPrice("7203", win)
	x := closeExecutor{broker: f.paper, posRepo: f.posRepo,
		closer: repository.NewCloser(f.posRepo, f.trades), emergency: f.es}
	if ok, err := x.closeOne(ctx, *p, win, port.CloseReasonRatchetTakeProfit, f.now); err != nil || !ok {
		t.Fatalf("closeOne: ok=%v err=%v", ok, err)
	}
	trs2, _ := f.trades.ListClosedSince(ctx, f.now.Add(-time.Hour))
	if len(trs2) != 1 || trs2[0].CloseReason != port.CloseReasonRatchetTakeProfit {
		t.Fatalf("close_reason = %+v, want ratchet_takeprofit", trs2)
	}
}

// ratchet 以外の理由は素通し(max_hold の損失を利確損に化けさせない)。
func TestCloseOneLeavesOtherReasonsAlone(t *testing.T) {
	ctx := context.Background()
	f := newSaga(t, nil)
	posID, err := f.enter(ctx, 100)
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	p, _ := f.posRepo.GetByID(ctx, posID)
	loss := p.EntryPrice - 10
	f.paper.SetPrice("7203", loss)
	x := closeExecutor{broker: f.paper, posRepo: f.posRepo,
		closer: repository.NewCloser(f.posRepo, f.trades), emergency: f.es}
	if ok, err := x.closeOne(ctx, *p, loss, "max_hold", f.now); err != nil || !ok {
		t.Fatalf("closeOne: ok=%v err=%v", ok, err)
	}
	trs, _ := f.trades.ListClosedSince(ctx, f.now.Add(-time.Hour))
	if len(trs) != 1 || trs[0].CloseReason != "max_hold" {
		t.Fatalf("close_reason = %+v, want max_hold", trs)
	}
}
