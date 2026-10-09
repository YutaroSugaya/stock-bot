package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/command"
)

// positionsBroker は GetPositions だけに答える(落とすか空を返すか)。
// MOCK rationale (TESTING.md 3用途): §1 system boundary — 建玉照会の失敗を注入する。
// Reconcile.Run は照会が落ちた時点で return するので他のメソッドには届かない。
type positionsBroker struct {
	port.Broker
	fail bool
}

func (b *positionsBroker) GetPositions(context.Context) ([]port.BrokerPosition, error) {
	if b.fail {
		return nil, errors.New("broker down")
	}
	return nil, nil
}

// 🚨 起動時 reconcile が落ちたら、その銘柄の新規 entry は **reconcile が 1 回成功するまで**
// 保留する。Warn だけ出して価格ループへ進むと、再起動直後の台帳が
// broker の建玉を知らないまま = ナンピン禁止ゲートが既存建玉を見落とす。
func TestStartupReconcile_FailureHoldsEntriesUntilASuccess(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	brk := &positionsBroker{fail: true}
	b := &SymbolBundle{
		Symbol:    "7203",
		Cycle:     command.NewTradingCycle(nil, nil, nil, 100, nil, tokyoHours()),
		Reconcile: command.NewReconcile(brk, repository.NewInMemoryPositionRepo(), nil, nil, nil, clock.Fixed(now)),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	b.StartupReconcile(ctx)
	if !b.Cycle.EntriesHeldForReconcile() {
		t.Fatal("起動時 reconcile が落ちたのに entry が保留されていない")
	}

	b.ReconcileTick(ctx) // まだ落ちている — 解除しない
	if !b.Cycle.EntriesHeldForReconcile() {
		t.Fatal("失敗した reconcile で保留が解けた")
	}

	brk.fail = false
	b.ReconcileTick(ctx)
	if b.Cycle.EntriesHeldForReconcile() {
		t.Fatal("reconcile が成功したのに保留が解けない")
	}
}

func TestStartupReconcile_SuccessDoesNotHold(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	b := &SymbolBundle{
		Symbol:    "7203",
		Cycle:     command.NewTradingCycle(nil, nil, nil, 100, nil, tokyoHours()),
		Reconcile: command.NewReconcile(&positionsBroker{}, repository.NewInMemoryPositionRepo(), nil, nil, nil, clock.Fixed(now)),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	b.StartupReconcile(context.Background())
	if b.Cycle.EntriesHeldForReconcile() {
		t.Fatal("成功した起動時 reconcile のあとも entry が保留されている")
	}
}

// reconcile を配線しない構成(Reconcile=nil)では保留しない — 解除する経路が無いので、
// 保留すると永久に建たない。
func TestStartupReconcile_NoReconcilerNeverHolds(t *testing.T) {
	b := &SymbolBundle{Symbol: "7203", Cycle: command.NewTradingCycle(nil, nil, nil, 100, nil, tokyoHours())}
	b.StartupReconcile(context.Background())
	if b.Cycle.EntriesHeldForReconcile() {
		t.Fatal("reconcile が無いのに保留した(解除経路が無い)")
	}
}
