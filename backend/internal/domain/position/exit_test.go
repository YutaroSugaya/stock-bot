package position_test

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
)

// entry 1000 の BUY なので UnrealizedJPY(price) == price-1000(出口幾何は円/株)。
func base(opened time.Time) position.Position {
	return position.Position{
		Side:       order.SideBuy,
		EntryPrice: 1000,
		Status:     position.StatusOpen,
		OpenedAt:   opened,
	}
}

// execute_order と同じく幅(円/株)と絶対価格の両方を凍結する。買いなので TP は上・SL は下。
func setTPSL(p *position.Position, tpJPY, slJPY float64) {
	p.TakeProfitJPY, p.StopLossJPY = tpJPY, slJPY
	if tpJPY > 0 {
		p.TakeProfitPrice = p.EntryPrice + tpJPY
	}
	if slJPY > 0 {
		p.StopLossPrice = p.EntryPrice - slJPY
	}
}

func TestEvaluateExit(t *testing.T) {
	now := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		mutate     func(p *position.Position)
		price      float64
		evalAt     time.Time
		wantExit   bool
		wantReason string
	}{
		{
			name:   "take_profit fires at target",
			mutate: func(p *position.Position) { setTPSL(p, 10, 10) },
			price:  1010, evalAt: now, wantExit: true, wantReason: "take_profit",
		},
		{
			name:   "stop_loss fires at target",
			mutate: func(p *position.Position) { setTPSL(p, 10, 10) },
			price:  990, evalAt: now, wantExit: true, wantReason: "stop_loss",
		},
		{
			name:   "no exit inside band",
			mutate: func(p *position.Position) { setTPSL(p, 10, 10) },
			price:  1005, evalAt: now, wantExit: false,
		},
		{
			name:   "max_hold fires past soft deadline",
			mutate: func(p *position.Position) { p.MaxHoldMinutes = 60 },
			price:  1000, evalAt: now.Add(61 * time.Minute), wantExit: true, wantReason: "max_hold",
		},
		{
			name: "max_hold extension holds near break-even",
			mutate: func(p *position.Position) {
				p.MaxHoldMinutes = 60
				p.ExtensionMaxMinutes = 30
				p.ExtensionUnrealizedJPY = 5
			},
			price: 1003, evalAt: now.Add(70 * time.Minute), wantExit: false,
		},
		{
			name: "max_hold extension expires at hard deadline",
			mutate: func(p *position.Position) {
				p.MaxHoldMinutes = 60
				p.ExtensionMaxMinutes = 30
				p.ExtensionUnrealizedJPY = 5
			},
			price: 1003, evalAt: now.Add(91 * time.Minute), wantExit: true, wantReason: "max_hold",
		},
		{
			name: "early_exit fires inside window at target",
			mutate: func(p *position.Position) {
				p.MaxHoldMinutes = 60
				p.EarlyExitWindowMinutes = 10
				p.EarlyExitTargetJPY = 3
			},
			price: 1004, evalAt: now.Add(55 * time.Minute), wantExit: true, wantReason: "early_exit",
		},
		{
			name: "ratchet take-profit on giveback when armed",
			mutate: func(p *position.Position) {
				p.RatchetArmJPY = 10
				p.RatchetGivebackJPY = 4
				p.PeakUnrealizedJPY = 12
				p.RatchetArmed = true
			},
			price: 1007, evalAt: now, wantExit: true, wantReason: "ratchet_takeprofit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base(now)
			tc.mutate(&p)
			got := position.EvaluateExit(p, tc.price, tc.evalAt)
			if got.Exit != tc.wantExit {
				t.Fatalf("Exit = %v, want %v (decision: %+v)", got.Exit, tc.wantExit, got)
			}
			if tc.wantExit && got.Reason != tc.wantReason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// giveback 無しの arm は決済せず、新しい peak を報告して呼び手に永続化させる(単調 ratchet)。
func TestEvaluateExit_RatchetArmsWithoutExit(t *testing.T) {
	now := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	p := base(now)
	p.RatchetArmJPY = 10
	p.RatchetGivebackJPY = 4

	got := position.EvaluateExit(p, 1012, now) // unreal = 12 → arms, peak=12, no giveback yet
	if got.Exit {
		t.Fatalf("unexpected exit: %+v", got)
	}
	if !got.ExcursionChanged || got.NewPeak != 12 || !got.NewArmed {
		t.Fatalf("ratchet state = %+v, want changed peak=12 armed=true", got)
	}
}
