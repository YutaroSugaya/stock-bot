package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/risk"
)

// 最低委託保証金は SnapshotCaps から snapshot へ運ばれ、保証金(キャッシュ経由の Equity)が
// 割っていれば entry を断る。paper の既定残高は 100 万円。
func TestTradingCycle_RejectsBelowMinimumCollateral(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	h.cycle.snapshot.caps.MinCollateralJPY = 2_000_000
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	res, err := h.cycle.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Entered || !strings.HasPrefix(res.RejectReason, risk.ReasonCollateralBelowMinimum) {
		t.Fatalf("保証金 100 万 < 下限 200 万で建った / 理由が違う: entered=%v reason=%q", res.Entered, res.RejectReason)
	}

	h.cycle.snapshot.caps.MinCollateralJPY = 1_000_000
	if res, _ := h.cycle.Execute(context.Background(), in); !res.Entered {
		t.Fatalf("下限ちょうどで建たない: %q", res.RejectReason)
	}
}
