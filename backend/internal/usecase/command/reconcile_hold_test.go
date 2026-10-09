package command

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
)

// 🚨 **起動時 reconcile が失敗したら、その銘柄の新規 entry を止める**。
//
// CLAUDE.md は「live は loop 開始前に同期 reconcile 1 周(再起動直後の盲点を塞ぐ)」と書くが、
// 実装は Warn を出して価格ループへ進んでいた。broker 照会が落ちた再起動では台帳が broker の
// 建玉を知らないまま = **ナンピン禁止ゲートが既存建玉を見落とす**。起動は止めない(守りの
// 置き直しと決済は動かし続ける)ので、止めるのは **その銘柄の新規 entry だけ**。
func TestTradingCycle_HoldsEntryUntilReconciled(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	rej := repository.NewInMemoryRejectionRepo()
	h.cycle.Rejections = rej
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	h.cycle.HoldEntriesUntilReconciled()
	res, err := h.cycle.Execute(ctx, in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Entered || res.RejectReason != ReasonReconcileUnconfirmed {
		t.Fatalf("reconcile 未成功の銘柄が建った / 理由が違う: entered=%v reason=%q", res.Entered, res.RejectReason)
	}
	if rows := rej.All(); len(rows) != 1 || rows[0].Reason != ReasonReconcileUnconfirmed {
		t.Fatalf("理由が signal_rejections に残らない: %+v", rows)
	}

	h.cycle.MarkReconciled()
	res, err = h.cycle.Execute(ctx, in)
	if err != nil || !res.Entered {
		t.Fatalf("reconcile 成功後も解除されない: entered=%v reason=%q err=%v", res.Entered, res.RejectReason, err)
	}
}

// 既定は保留しない(paper / backtest / reconcile を回さない配線の挙動を変えない)。
func TestTradingCycle_DoesNotHoldByDefault(t *testing.T) {
	h := newHarness(t, time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST))
	if h.cycle.EntriesHeldForReconcile() {
		t.Fatal("既定で entry を保留している")
	}
}
