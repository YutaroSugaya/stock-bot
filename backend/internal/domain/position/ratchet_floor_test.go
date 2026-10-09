package position

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
)

// トレール利確が **armed 後に建値割れで終わる**構成は、名前と実装が
// 矛盾している。giveback(1.5×ATR)が arm(1.0×ATR)より大きい限り、peak が
// 1.0〜1.5×ATR の帯で armed になった玉は**構造的に必ず損で出る**(最悪 −0.5×ATR)。
// 根拠は損益ではなく機構 — 1 本も約定を見なくても指摘できる。
//
// 新規則: floor = max(RatchetArmJPY, peak − RatchetGivebackJPY)

func floorPos(arm, giveback, peak float64, armed, floorAtArm bool) Position {
	return Position{
		ID: 1, Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		EntryPrice:         2000,
		RatchetArmJPY:      arm,
		RatchetGivebackJPY: giveback,
		PeakUnrealizedJPY:  peak,
		RatchetArmed:       armed,
		RatchetFloorAtArm:  floorAtArm,
		Status:             StatusOpen,
		OpenedAt:           time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC),
	}
}

func atUnrealized(p Position, unreal float64) float64 {
	// BUY: price = entry + unreal。SELL は PriceForUnrealized が符号を反転する。
	return p.PriceForUnrealized(unreal)
}

// 4901(実測): 含み +219円/株 まで乗ってから giveback 線(peak−1.5ATR)で
// gross ほぼ 0 で決済した。新規則では床 = arm = 143.29 で止まる。
func TestRatchetFloorStopsGivebackAtArmLevel(t *testing.T) {
	const arm, giveback = 143.29, 214.93
	p := floorPos(arm, giveback, 219, true, true)
	now := p.OpenedAt.Add(time.Hour)

	// 旧規則なら peak−giveback = +4.07 まで落ちても決済しない。新規則は arm で切る。
	justAbove := arm + 0.01
	if d := EvaluateExit(p, atUnrealized(p, justAbove), now); d.Exit {
		t.Fatalf("床のわずかに上では決済しないこと: reason=%q", d.Reason)
	}
	if d := EvaluateExit(p, atUnrealized(p, arm), now); !d.Exit {
		t.Fatal("床(= arm 水準)に触れたら決済すること")
	}
	// 床に**触れた**時点で出る(< ではなく <=)。旧規則の (peak-unreal)>=giveback と
	// 同じ比較の向きを保つ、という決定を固定する(境界 (a))。
	if d := EvaluateExit(p, atUnrealized(p, arm), now); d.Reason != "ratchet_takeprofit" {
		t.Fatalf("reason = %q, want ratchet_takeprofit", d.Reason)
	}
}

// peak ≥ 2.5×ATR(= arm + giveback)では旧規則と**完全に同じ**。変わるのは
// peak が 1.0〜2.5×ATR の帯だけ。
func TestRatchetFloorIdenticalToOldRuleAboveArmPlusGiveback(t *testing.T) {
	const arm, giveback float64 = 100, 150
	peak := arm + giveback + 40 // 床は peak−giveback = 140 > arm
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)

	newRule := floorPos(arm, giveback, peak, true, true)
	oldRule := floorPos(arm, giveback, peak, true, false)
	for _, unreal := range []float64{peak - giveback + 1, peak - giveback, peak - giveback - 1} {
		gotNew := EvaluateExit(newRule, atUnrealized(newRule, unreal), now).Exit
		gotOld := EvaluateExit(oldRule, atUnrealized(oldRule, unreal), now).Exit
		if gotNew != gotOld {
			t.Fatalf("unreal=%v: 新旧で挙動が違う(new=%v old=%v) — この帯では同一のはず",
				unreal, gotNew, gotOld)
		}
	}
}

// 🛑 床は Position の凍結値から**実行時に計算する**ので、フラグが無いと
// harvest トラックで自然決済を待っている旧建玉にもその場で新規則が効く。
// false の建玉は旧規則のままでなければならない(測定対象を途中で入れ替えない)。
func TestRatchetFloorDoesNotApplyToPositionsFrozenUnderTheOldRule(t *testing.T) {
	const arm, giveback = 143.29, 214.93
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	old := floorPos(arm, giveback, 219, true, false)

	// 旧規則: peak−giveback = +4.07 まで落ちて初めて決済する。
	if d := EvaluateExit(old, atUnrealized(old, arm), now); d.Exit {
		t.Fatal("false の建玉に床が効いている — 測定対象が途中で入れ替わる")
	}
	if d := EvaluateExit(old, atUnrealized(old, 219-giveback), now); !d.Exit {
		t.Fatal("旧規則の giveback 線で決済しないのは回帰")
	}
}

// 境界 (b): 床・peak・unreal は**円/株の符号付き**。SELL 建玉でも同じ式。
func TestRatchetFloorWorksForShortPositions(t *testing.T) {
	const arm, giveback float64 = 100, 150
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	p := floorPos(arm, giveback, 120, true, true)
	p.Side = order.SideSell

	// SELL は値下がりが利益。含み +120 = 価格 1880。床 = arm = 100 → 価格 1900。
	if got := atUnrealized(p, 100); got != 1900 {
		t.Fatalf("前提: SELL の含み +100 は価格 1900(got %v)", got)
	}
	if d := EvaluateExit(p, atUnrealized(p, 101), now); d.Exit {
		t.Fatal("SELL: 床の上では決済しないこと")
	}
	if d := EvaluateExit(p, atUnrealized(p, 100), now); !d.Exit {
		t.Fatal("SELL: 床に触れたら決済すること(価格比較ではなく符号付き含みで判定)")
	}
}

// armed 前は一切変わらない(SL も arm 判定も不変)。
func TestRatchetFloorDoesNotChangePreArmBehaviour(t *testing.T) {
	const arm, giveback float64 = 100, 150
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	p := floorPos(arm, giveback, 40, false, true) // peak が arm 未満 = 未 arm
	d := EvaluateExit(p, atUnrealized(p, 30), now)
	if d.Exit {
		t.Fatalf("未 arm で決済している: reason=%q", d.Reason)
	}
	if d.NewArmed {
		t.Fatal("peak < arm なのに armed になっている")
	}
}

// 床に到達したのと同じティックで初めて armed になる場合(境界 (a))。
// peak == arm ちょうどで armed になり、unreal == peak == 床 なので**その場で決済する**。
// 「+1.0×ATR の利確」であり損ではない — 挙動として固定しておく。
func TestRatchetFloorArmAndExitOnTheSameTick(t *testing.T) {
	const arm, giveback float64 = 100, 150
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	p := floorPos(arm, giveback, 0, false, true) // まだ armed でない
	d := EvaluateExit(p, atUnrealized(p, arm), now)
	if !d.NewArmed {
		t.Fatal("peak が arm に達したら armed になること")
	}
	if !d.Exit || d.Reason != "ratchet_takeprofit" {
		t.Fatalf("同一ティックで床に触れているので決済する: exit=%v reason=%q", d.Exit, d.Reason)
	}
}

// 画面と broker に出る守りの値段(ProtectiveExit)が、実際に決済する線(EvaluateExit)と
// ずれないこと。片方だけ旧式のままだと「表示は +4 円だが実際は +143 円で切れる」になる。
func TestProtectiveExitUsesTheSameFloorAsEvaluateExit(t *testing.T) {
	const arm, giveback float64 = 143.29, 214.93
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	p := floorPos(arm, giveback, 219, true, true)
	p.StopLossPrice = 1500 // SL は床より遠い(= 床が守りの線になる)

	line, reason := ProtectiveExit(p)
	if reason != "ratchet_takeprofit" {
		t.Fatalf("守りの理由 = %q, want ratchet_takeprofit", reason)
	}
	if !EvaluateExit(p, line, now).Exit {
		t.Fatalf("ProtectiveExit が示す値段 %v で EvaluateExit が決済しない — 2 つの線がずれている", line)
	}
	if got := p.PriceForUnrealized(arm); line != got {
		t.Fatalf("守りの線 = %v, want %v(床 = arm 水準)", line, got)
	}
}

// 🚨 **売り建玉 × 床 × ProtectiveExit の交差点**(売りが開いた後に新設された
// 組合せ)。
//
// 床は side 補正済みの円/株で定義されているので、SELL でも同じ式で成り立つ
// (価格の大小で書き直すと空売りで反転する)。ProtectiveExit が返す**値段**が
// EvaluateExit の決済線と一致することを、買いと同じ強さで縛る。
func TestProtectiveExitFloorForShortPositions(t *testing.T) {
	const arm, giveback float64 = 143.29, 214.93
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	p := floorPos(arm, giveback, 219, true, true)
	p.Side = order.SideSell
	// 売りの SL は建値より**上**。床より遠くに置いて、床が守りの線になるようにする。
	p.StopLossPrice = p.EntryPrice + 1500

	line, reason := ProtectiveExit(p)
	if reason != "ratchet_takeprofit" {
		t.Fatalf("守りの理由 = %q, want ratchet_takeprofit", reason)
	}
	// 🛑 売りの利確線は建値より**下**。ここが上に出るなら side 補正が抜けている。
	if line >= p.EntryPrice {
		t.Fatalf("売りの守りの線 = %v が建値 %v 以上 — side 補正が抜けている", line, p.EntryPrice)
	}
	if !EvaluateExit(p, line, now).Exit {
		t.Fatalf("ProtectiveExit が示す値段 %v で EvaluateExit が決済しない — 2 つの線がずれている", line)
	}
	if got := p.PriceForUnrealized(arm); line != got {
		t.Fatalf("守りの線 = %v, want %v(床 = arm 水準)", line, got)
	}
	// 床の 1 ティック手前(= まだ含みが床より上)では出ない。
	if EvaluateExit(p, line-1, now).Exit {
		t.Fatal("床に触れる前に決済した")
	}
}
