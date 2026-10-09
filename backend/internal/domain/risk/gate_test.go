package risk

import (
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

func passingSignal() strategy.Signal {
	return strategy.Signal{
		Decision:      strategy.DecisionEnter,
		Side:          order.SideBuy,
		EntryPrice:    2500, // tick 1
		TakeProfitJPY: 100,  // -> 2600 aligned
		StopLossJPY:   50,   // -> 2450 aligned
		Quantity:      100,
		HoldingMode:   order.HoldingIntraday,
	}
}

func passingConfig() *config.StrategyConfig {
	c := &config.StrategyConfig{ExecKind: config.ExecMarginOneday}
	c.Entry.Direction = config.DirectionBoth
	c.Entry.MaxSpreadTicks = 5
	c.Risk.MaxOpenPositions = 1
	return c
}

func passingSummary() *market.MarketSummary {
	return &market.MarketSummary{CurrentRate: market.CurrentRate{SpreadTicks: 1}}
}

func TestEvaluateSignal_AllowsCleanEntry(t *testing.T) {
	d := EvaluateSignal(passingSignal(), passingConfig(), AccountSnapshot{}, passingSummary())
	if !d.Allowed {
		t.Fatalf("clean entry rejected: %q", d.Reason)
	}
}

func TestEvaluateSignal_Guards(t *testing.T) {
	cases := []struct {
		name       string
		mutSnap    func(*AccountSnapshot)
		mutSig     func(*strategy.Signal)
		mutCfg     func(*config.StrategyConfig)
		wantReason string
	}{
		{"emergency_stop", func(s *AccountSnapshot) { s.EmergencyStop = true }, nil, nil, "emergency_stop"},
		{"outside_session", func(s *AccountSnapshot) { s.OutsideSessionHours = true }, nil, nil, "outside_session_hours"},
		{"eod_flat", func(s *AccountSnapshot) { s.EndOfDayCloseRequired = true }, nil, nil, "end_of_day_flat_required"},
		{"daily_loss", func(s *AccountSnapshot) { s.MaxDailyLossJPY = 5000; s.DailyLossJPY = 5000 }, nil, nil, "daily_loss 5000 >= cap 5000"},
		{"account_daily_loss", func(s *AccountSnapshot) { s.AccountMaxDailyLossJPY = 18000; s.AccountDailyLossJPY = 18000 }, nil, nil, "account_daily_loss 18000 >= cap 18000"},
		{"insufficient_margin", func(s *AccountSnapshot) { s.CollateralRequiredJPY = 300000; s.AvailableToTradeJPY = 100000 }, nil, nil, "insufficient_margin need 300000 > avail 100000"},
		// 不足金 → avail 0/negative must REJECT, not self-disable the gate: the
		// adapter deliberately reports 0 available when collateral is exhausted.
		{"insufficient_margin_zero_avail", func(s *AccountSnapshot) { s.CollateralRequiredJPY = 300000; s.AvailableToTradeJPY = 0 }, nil, nil, "insufficient_margin need 300000 > avail 0"},
		{"insufficient_margin_negative_avail", func(s *AccountSnapshot) { s.CollateralRequiredJPY = 300000; s.AvailableToTradeJPY = -50000 }, nil, nil, "insufficient_margin need 300000 > avail -50000"},
		{"margin_status_unavailable", func(s *AccountSnapshot) { s.MarginStatusUnknown = true }, nil, nil, "margin_status_unavailable"},
		{"risk_state_unavailable", func(s *AccountSnapshot) { s.RepoStatusUnknown = true }, nil, nil, "risk_state_unavailable"},
		// 枠のキーが (銘柄, 戦略) なので、**同一戦略**の
		// 建玉として両方の counter を立てる。「別戦略なら通る / live は従来どおり通さない」は
		// position_cap_per_strategy_test.go が別に固定する。
		{"open_positions", func(s *AccountSnapshot) { s.OpenPositions = 1; s.OpenPositionsSameStrategy = 1 }, nil, nil, "open_positions 1 >= cap 1"},
		{"account_open_positions", func(s *AccountSnapshot) { s.AccountOpenPositions = 3; s.AccountMaxOpenPositions = 3 }, nil, nil, "account_open_positions 3 >= cap 3"},
		// キーが (銘柄, 側, 戦略) なので、**同一戦略**の
		// 建玉として両方の counter を立てる。「別戦略なら通る / live は従来どおり通さない」は
		// nanpin_per_strategy_test.go が別に固定する。
		{"nanpin_buy", func(s *AccountSnapshot) { s.OpenBuyInclExternal = 1; s.OpenBuySameStrategyInclExternal = 1 }, nil, func(c *config.StrategyConfig) { c.Risk.MaxOpenPositions = 5 }, "pyramiding_blocked_same_side_buy (1 open incl external, cap 1)"},
		{"consecutive_losses", func(s *AccountSnapshot) { s.MaxConsecutiveLosses = 4; s.ConsecutiveLosses = 4 }, nil, nil, "consecutive_losses 4 >= cap 4"},
		{"trades_in_window", nil, nil, func(c *config.StrategyConfig) { c.Risk.MaxTradesInThisWindow = 3 }, "trades_in_window 3 >= cap 3"},
		{"direction_buy_only_blocks_short", nil, func(s *strategy.Signal) { s.Side = order.SideSell }, func(c *config.StrategyConfig) { c.Entry.Direction = config.DirectionBuyOnly }, "direction_buy_only_blocks_short"},
		{"oneday_multiday_conflict", nil, func(s *strategy.Signal) { s.HoldingMode = order.HoldingMultiday }, nil, "oneday_margin_cannot_hold_multiday"},
		{"spread_too_wide", func(s *AccountSnapshot) {}, nil, func(c *config.StrategyConfig) { c.Entry.MaxSpreadTicks = 1 }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := passingSignal()
			cfg := passingConfig()
			snap := AccountSnapshot{}
			summary := passingSummary()
			if tc.name == "trades_in_window" {
				snap.TradesInWindow = 3
			}
			if tc.name == "spread_too_wide" {
				summary.CurrentRate.SpreadTicks = 10
			}
			if tc.mutSnap != nil {
				tc.mutSnap(&snap)
			}
			if tc.mutSig != nil {
				tc.mutSig(&sig)
			}
			if tc.mutCfg != nil {
				tc.mutCfg(cfg)
			}
			d := EvaluateSignal(sig, cfg, snap, summary)
			if d.Allowed {
				t.Fatalf("expected reject, got allowed")
			}
			if tc.wantReason != "" && d.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", d.Reason, tc.wantReason)
			}
		})
	}
}

func TestEvaluateSignal_QtyHalvedAfterTwoLosses(t *testing.T) {
	snap := AccountSnapshot{ConsecutiveLosses: 2}
	d := EvaluateSignal(passingSignal(), passingConfig(), snap, passingSummary())
	if !d.Allowed || d.QtyMultiplier != 0.5 {
		t.Fatalf("want allowed with 0.5 multiplier, got allowed=%v mul=%g", d.Allowed, d.QtyMultiplier)
	}
}

func TestEvaluateHardSafety_CannotBypassHardGates(t *testing.T) {
	// Overridable guards (open_positions) are ignored by HardSafety...
	snap := AccountSnapshot{OpenPositions: 99}
	if d := EvaluateHardSafety(passingSignal(), passingConfig(), snap, passingSummary()); !d.Allowed {
		t.Fatalf("hard safety should ignore open_positions, got %q", d.Reason)
	}
	// ...but daily_loss / emergency / session / margin are never bypassed.
	for _, mut := range []func(*AccountSnapshot){
		func(s *AccountSnapshot) { s.EmergencyStop = true },
		func(s *AccountSnapshot) { s.OutsideSessionHours = true },
		func(s *AccountSnapshot) { s.MaxDailyLossJPY = 5000; s.DailyLossJPY = 5000 },
		func(s *AccountSnapshot) { s.MarginStatusUnknown = true },
		func(s *AccountSnapshot) { s.RepoStatusUnknown = true },
	} {
		s := AccountSnapshot{Now: time.Now()}
		mut(&s)
		if d := EvaluateHardSafety(passingSignal(), passingConfig(), s, passingSummary()); d.Allowed {
			t.Fatal("hard safety allowed a hard-gated entry")
		}
	}
}
