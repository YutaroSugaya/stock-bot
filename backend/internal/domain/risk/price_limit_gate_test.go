package risk

import (
	"testing"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

// live 事故の回帰。25日線 6,435 の TP で建てようとしたが、当日の
// 値幅制限上限は 6,352(基準 5,352 ± 1,000)。守りの発注が立花に拒否され、約定済みの
// 建玉を閉じる → 台帳に何も残らない → 再入場、を 5 往復繰り返した。
//
// TP が帯の外なのは**機会損失**(TP 脚を落として OnTick で見る)。
// SL が帯の外なのは**致命的**(守りを board に置けない)ので建てない。
func sigFor(tpJPY, slJPY float64) strategy.Signal {
	return strategy.Signal{
		Decision: strategy.DecisionEnter, Symbol: "4704", Side: order.SideBuy,
		EntryPrice: 5381, TakeProfitJPY: tpJPY, StopLossJPY: slJPY, Quantity: 100,
	}
}

// SL が下限の内側なら通す(TP が上限の外でも entry は止めない)。
func TestEvaluatePriceLimit_AllowsWhenStopIsInsideBand(t *testing.T) {
	// 基準 5,352 → 帯 4,352〜6,352。SL 4,798 は内側、TP 6,435 は外側。
	d := EvaluatePriceLimit(sigFor(1054.36, 582.56), 5352)
	if !d.Allowed {
		t.Fatalf("SL が帯の内側なら通すこと: %+v", d)
	}
}

// SL が下限の外なら建てない — 守りを broker 側に置けない建玉は作らない。
func TestEvaluatePriceLimit_RejectsStopBelowLimitDown(t *testing.T) {
	// SL 幅 1,100 → 5,381-1,100 = 4,281 < 下限 4,352。
	d := EvaluatePriceLimit(sigFor(1054.36, 1100), 5352)
	if d.Allowed {
		t.Fatal("SL が値幅制限の外なのに通した — 守りを置けない建玉ができる")
	}
	if d.Reason != "stop_loss_outside_price_limit" {
		t.Fatalf("Reason = %q, want stop_loss_outside_price_limit", d.Reason)
	}
}

// 売り建ての SL は上側。対称に効くこと。
func TestEvaluatePriceLimit_RejectsShortStopAboveLimitUp(t *testing.T) {
	s := sigFor(0, 1100)
	s.Side = order.SideSell
	d := EvaluatePriceLimit(s, 5352) // 5,381+1,100 = 6,481 > 上限 6,352
	if d.Allowed {
		t.Fatal("売り建ての SL が上限の外なのに通した")
	}
}

// 基準値段が無ければ fail-close。「確かめられない」を「大丈夫」と読まない。
func TestEvaluatePriceLimit_FailsClosedWithoutReference(t *testing.T) {
	d := EvaluatePriceLimit(sigFor(1054.36, 582.56), 0)
	if d.Allowed {
		t.Fatal("基準値段ゼロで通した — 値幅制限を確かめられないまま建ててはいけない")
	}
	if d.Reason != "price_limit_reference_unavailable" {
		t.Fatalf("Reason = %q, want price_limit_reference_unavailable", d.Reason)
	}
}

// SL 幅ゼロ(守り無し)はこのゲートの対象外 — 別のゲートが弾く。ここで誤って
// 「0 は帯の内側」と通してしまわないことだけ固定する。
func TestEvaluatePriceLimit_ZeroStopIsNotThisGatesJob(t *testing.T) {
	if d := EvaluatePriceLimit(sigFor(1054.36, 0), 5352); !d.Allowed {
		t.Fatalf("SL 幅 0 はこのゲートの担当外(通す): %+v", d)
	}
}

// 🛑 TP が帯の外でも entry は止めない。止めると「急落が深いほど TP が遠い」BNF の
// 一番強いシグナルだけが系統的に消え、標本が歪む。TP 脚は発注側で落とす。
func TestEvaluatePriceLimit_TakeProfitOutsideDoesNotBlockEntry(t *testing.T) {
	if d := EvaluatePriceLimit(sigFor(5000, 582.56), 5352); !d.Allowed {
		t.Fatalf("TP が帯の外でも entry は通すこと: %+v", d)
	}
}

// 発注側が使う判定: TP 脚を board に置けるか。
func TestProtectiveTakeProfitPlaceable(t *testing.T) {
	cases := []struct {
		name    string
		side    order.Side
		tp, ref float64
		want    bool
	}{
		{"買い建て・上限の内側", order.SideBuy, 6300, 5352, true},
		{"買い建て・上限の外", order.SideBuy, 6435, 5352, false},
		{"買い建て・上限ちょうど", order.SideBuy, 6352, 5352, true},
		{"売り建て・下限の外", order.SideSell, 4300, 5352, false},
		{"売り建て・下限の内側", order.SideSell, 4400, 5352, true},
		{"TP 無し", order.SideBuy, 0, 5352, false},
		{"基準値段なし", order.SideBuy, 6300, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ProtectiveTakeProfitPlaceable(c.side, c.tp, c.ref); got != c.want {
				t.Fatalf("ProtectiveTakeProfitPlaceable(%v, %v, %v) = %v, want %v", c.side, c.tp, c.ref, got, c.want)
			}
		})
	}
}
