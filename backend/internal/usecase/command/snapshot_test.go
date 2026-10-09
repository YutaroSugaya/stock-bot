package command

import (
	"context"
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/strategy"
)

type errPosRepo struct {
	*repository.InMemoryPositionRepo
}

func (errPosRepo) ListOpenOrClosing(context.Context, string) ([]position.Position, error) {
	return nil, errors.New("pg: connection reset")
}

// A snapshot that cannot read the risk state must FAIL CLOSE, or the
// never-overridable caps (nanpin, daily-loss) are evaluated against a silent zero.
func TestSnapshotBuilder_FailsCloseOnRepoError(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 1, 0)
	posRepo := errPosRepo{repository.NewInMemoryPositionRepo()}
	tradeRepo := repository.NewInMemoryTradeRepo()

	sb := NewSnapshotBuilder(posRepo, tradeRepo, pb, nil, tokyoHours(), c, SnapshotCaps{RequiredMarginRate: 0.30})
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "7203", EntryPrice: 2500, Quantity: 100}

	snap := sb.BuildStructural(context.Background(), "7203", nil, sig, order.ExecCash)
	if !snap.RepoStatusUnknown {
		t.Fatal("a failed position read must set RepoStatusUnknown (fail-close), not be swallowed")
	}
}

func TestSnapshotBuilder_HealthyReadIsNotTainted(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	c := clock.Fixed(now)
	pb := broker.NewPaper(c, 1, 0)
	posRepo := repository.NewInMemoryPositionRepo()
	tradeRepo := repository.NewInMemoryTradeRepo()

	sb := NewSnapshotBuilder(posRepo, tradeRepo, pb, nil, tokyoHours(), c, SnapshotCaps{RequiredMarginRate: 0.30})
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "7203", EntryPrice: 2500, Quantity: 100}

	snap := sb.BuildStructural(context.Background(), "7203", nil, sig, order.ExecCash)
	if snap.RepoStatusUnknown {
		t.Fatal("a healthy snapshot must not be marked RepoStatusUnknown")
	}
}
