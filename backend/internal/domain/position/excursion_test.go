package position_test

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
)

// 建玉の最大順行(peak)/ 最大逆行(trough)は **全建玉**で記録する。ratchet を持つ
// 建玉でしか更新していなかったため、非ゼロの peak は決済済み 384 本中 9 本しか無く、
// トレール反実仮想は5分足からの再構築(誤差数%)に頼るしかなかった。
// **決済規則は変えない — 計測器の追加**。
func TestEvaluateExit_TracksExcursionWithoutRatchet(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	p := base(now)
	setTPSL(&p, 100, 100) // どちらにも当たらない値幅にして、記録だけを見る

	up := position.EvaluateExit(p, 1030, now)
	if up.Exit {
		t.Fatalf("must not exit inside the band: %+v", up)
	}
	if !up.ExcursionChanged {
		t.Fatal("a new favourable extreme must be reported for persistence")
	}
	if up.NewPeak != 30 {
		t.Fatalf("NewPeak = %v, want 30", up.NewPeak)
	}
	if up.NewTrough != 0 {
		t.Fatalf("NewTrough = %v, want 0 (price never went below entry)", up.NewTrough)
	}

	p.PeakUnrealizedJPY, p.TroughUnrealizedJPY = up.NewPeak, up.NewTrough
	down := position.EvaluateExit(p, 980, now)
	if down.Exit {
		t.Fatalf("must not exit inside the band: %+v", down)
	}
	if down.NewPeak != 30 {
		t.Fatalf("NewPeak = %v, want the peak to stay at 30 (a drawdown does not erase it)", down.NewPeak)
	}
	if down.NewTrough != -20 {
		t.Fatalf("NewTrough = %v, want -20 (MAE: 「SL がもう少し広ければ助かったか」の材料)", down.NewTrough)
	}

	p.PeakUnrealizedJPY, p.TroughUnrealizedJPY = down.NewPeak, down.NewTrough
	flat := position.EvaluateExit(p, 1010, now)
	if flat.ExcursionChanged {
		t.Fatalf("no new extreme = no write: %+v", flat)
	}
}

// The SELL side mirrors: 順行 = 値下がり。
func TestEvaluateExit_TracksExcursionShortSide(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	p := base(now)
	p.Side = order.SideSell
	p.TakeProfitPrice, p.StopLossPrice = 900, 1100

	d := position.EvaluateExit(p, 970, now)
	if d.NewPeak != 30 {
		t.Fatalf("NewPeak = %v, want 30 (short gains when price falls)", d.NewPeak)
	}
	p.PeakUnrealizedJPY = d.NewPeak
	d = position.EvaluateExit(p, 1040, now)
	if d.NewTrough != -40 {
		t.Fatalf("NewTrough = %v, want -40", d.NewTrough)
	}
}

// 決済規則は不変: ratchet 建玉の arm / giveback の挙動は従来どおり。
func TestEvaluateExit_RatchetUnchangedByExcursionTracking(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	p := base(now)
	setTPSL(&p, 0, 100)
	p.RatchetArmJPY, p.RatchetGivebackJPY = 20, 10

	d := position.EvaluateExit(p, 1025, now) // arms
	if d.Exit || !d.NewArmed || d.NewPeak != 25 {
		t.Fatalf("arming tick changed: %+v", d)
	}
	p.PeakUnrealizedJPY, p.RatchetArmed = d.NewPeak, d.NewArmed

	if d := position.EvaluateExit(p, 1014, now); !d.Exit || d.Reason != "ratchet_takeprofit" {
		t.Fatalf("giveback must still close the position: %+v", d)
	}
}

// C1: **決済する tick の極値も記録する**。SL に当たった瞬間の逆行が MAE の本命で、
// 閉じた後には観測できない。`if dec.Exit { continue }` に戻すとここが赤くなる。
func TestEvaluateExit_ExitTickStillReportsExcursion(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	p := base(now)
	setTPSL(&p, 100, 20)

	d := position.EvaluateExit(p, 980, now) // SL ちょうど
	if !d.Exit || d.Reason != "stop_loss" {
		t.Fatalf("決済規則が変わっている: %+v", d)
	}
	if !d.ExcursionChanged {
		t.Fatal("決済 tick の極値が記録されない(閉じた後には観測できない)")
	}
	if d.NewTrough != -20 {
		t.Fatalf("NewTrough = %v, want -20(SL に当たった瞬間の逆行)", d.NewTrough)
	}
}

// ratchet の決済 tick も同じ。
func TestEvaluateExit_RatchetExitReportsExcursion(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	p := base(now)
	setTPSL(&p, 0, 100)
	p.RatchetArmJPY, p.RatchetGivebackJPY = 20, 10
	p.PeakUnrealizedJPY, p.RatchetArmed = 25, true

	d := position.EvaluateExit(p, 1014, now) // peak 25 → 14 で giveback 到達
	if !d.Exit || d.Reason != "ratchet_takeprofit" {
		t.Fatalf("決済規則が変わっている: %+v", d)
	}
	if d.NewPeak != 25 {
		t.Fatalf("NewPeak = %v, want 25", d.NewPeak)
	}
}
