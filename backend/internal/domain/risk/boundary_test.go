package risk

import (
	"testing"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

func boundarySig(entry, slTicks, tpTicks float64) strategy.Signal {
	return strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "7203",
		EntryPrice: entry, StopLossJPY: slTicks, TakeProfitJPY: tpTicks, Quantity: 100,
	}
}

func TestEvaluateOrderBoundary(t *testing.T) {
	// 出口幅は円/株。損失 = 幅 × 株数(呼値は関与しない)。%上限は建値比。
	b := OrderBoundary{MaxStopLossPct: 12, MaxTakeProfitPct: 40, MaxLossPerTradeJPY: 30000}

	cases := []struct {
		name    string
		sig     strategy.Signal
		qty     int
		b       OrderBoundary
		wantOK  bool
		wantWhy string
	}{
		{"bnf_within_boundary", boundarySig(2000, 160, 400), 100, b, true, ""},                              // SL 8% / TP 20% / 損失 16,000
		{"sl_pct_exceeds", boundarySig(2000, 300, 400), 100, b, false, "stop_loss_pct_exceeds_boundary"},    // SL 15% > 12%
		{"tp_pct_exceeds", boundarySig(2000, 100, 1000), 100, b, false, "take_profit_pct_exceeds_boundary"}, // TP 50% > 40%
		{"loss_jpy_exceeds", boundarySig(2000, 200, 400), 200, b, false, "max_loss_per_trade_exceeded"},     // 200円 × 200株 = 40,000 > 30,000
		// 同じ幅でも株数が増えれば損失 cap に当たる(サイジングが効いていることの固定)。
		{"same_width_smaller_lot_passes", boundarySig(2000, 200, 400), 100, b, true, ""}, // 20,000 < 30,000
		{"zero_boundary_disables", boundarySig(2000, 9999, 9999), 9999, OrderBoundary{}, true, ""},
		{"tight_boundary_rejects_bnf", boundarySig(2000, 160, 400), 100,
			OrderBoundary{MaxStopLossPct: 5, MaxTakeProfitPct: 200, MaxLossPerTradeJPY: 8000}, false, "stop_loss_pct_exceeds_boundary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := EvaluateOrderBoundary(tc.sig, tc.qty, tc.b)
			if d.Allowed != tc.wantOK {
				t.Fatalf("Allowed = %v, want %v (reason %q)", d.Allowed, tc.wantOK, d.Reason)
			}
			if !tc.wantOK && d.Reason != tc.wantWhy {
				t.Fatalf("reason = %q, want %q", d.Reason, tc.wantWhy)
			}
		})
	}
}

// 出口幅が円建てなので境界判定は呼値に依存しない(tick 建てだった頃はここが最大10倍ずれていた)。
func TestEvaluateOrderBoundaryIsTickSizeIndependent(t *testing.T) {
	b := OrderBoundary{MaxStopLossPct: 12, MaxTakeProfitPct: 40, MaxLossPerTradeJPY: 30000}
	fine := boundarySig(2000, 200, 400)   // 7203 = TOPIX500(呼値 0.5円)
	coarse := boundarySig(2000, 200, 400) // ユニバース外(呼値 1円)
	coarse.Symbol = "9999"
	if EvaluateOrderBoundary(fine, 200, b) != EvaluateOrderBoundary(coarse, 200, b) {
		t.Fatal("呼値で判定が変わっている(出口幅は円建てのはず)")
	}
}

// 出口が ATR 倍数なので、fat-finger 天井が戦略の幾何を削らないことを固定する。
// 観測(運用データ222銘柄): 2×ATR は終値比の中央値 7.0% / p90 11.9% / 最大 27.2%(6976)。
// 旧 max_stop_loss_pct=12 は固定8%SL を通すための値で、そのままだと 14銘柄のエントリーが
// シグナルの後に黙って弾かれる — しかも落ちるのは最高ボラ = BNF がパニックで発火する場面そのもの。
func TestOrderBoundaryAdmitsATRGeometryOnHighVolNames(t *testing.T) {
	// 実測最大ケース: ATR が終値の 13.6%(6976 相当)→ SL=2×ATR=27.2% / TP=3×ATR=40.8%
	const entry = 10000.0
	atr := entry * 0.136
	sig := strategy.Signal{EntryPrice: entry, StopLossJPY: 2.0 * atr, TakeProfitJPY: 3.0 * atr}
	b := OrderBoundary{MaxStopLossPct: 30, MaxTakeProfitPct: 45, MaxLossPerTradeJPY: 1_000_000}
	if d := EvaluateOrderBoundary(sig, 100, b); !d.Allowed {
		t.Fatalf("ATR 幾何が fat-finger 天井に削られている: %s(SL %.1f%% / TP %.1f%%)",
			d.Reason, sig.StopLossJPY/entry*100, sig.TakeProfitJPY/entry*100)
	}
	// 天井は R:R 1.5 と整合していること(片方だけ動かすと「SL は通るが TP で落ちる」不整合になる)。
	if b.MaxTakeProfitPct != b.MaxStopLossPct*1.5 {
		t.Fatalf("TP 上限 %.0f は SL 上限 %.0f × 1.5 でない — 出口の R:R と天井がズレる",
			b.MaxTakeProfitPct, b.MaxStopLossPct)
	}
	// それでも「桁違い」は捕まえること(天井を無効化したわけではない)。
	absurd := strategy.Signal{EntryPrice: entry, StopLossJPY: entry * 0.5}
	if d := EvaluateOrderBoundary(absurd, 100, b); d.Allowed {
		t.Fatal("建値の50%のストップは fat-finger として弾くこと")
	}
}
