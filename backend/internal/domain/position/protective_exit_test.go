package position_test

import (
	"testing"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
)

// トレール建玉は損失側の脚を2本持つ(giveback 線と SL)。遠い方の線には価格が先に届かない。
// 高値株の例: giveback 21,894.6円 に対し SL は建値の8% = 3,070.4円 しかなく、
// ピーク +10,890円 時点のトレール線 30,045.4 は SL 37,980 の**外側** = 到達し得ない値段になる。
// 画面がそれを「決済ライン」と出すと、直したはずの誤読を再生産する。
func TestProtectiveExit(t *testing.T) {
	// 高値株の凍結値の例。
	trail285A := position.Position{
		Side: order.SideBuy, Quantity: 100, EntryPrice: 41050,
		StopLossPrice: 37980, RatchetArmJPY: 8757.857142857143,
		RatchetGivebackJPY: 21894.64285714286,
	}

	// 定数式は Go が任意精度で畳み込むので、期待値も float64 の実演算で作る。
	entry, give := trail285A.EntryPrice, trail285A.RatchetGivebackJPY
	peakInsideSL := 25000.0
	wantRatchetLine := entry + peakInsideSL - give // = 44155.4 > SL 37980

	tests := []struct {
		name       string
		mutate     func(p *position.Position)
		wantPrice  float64
		wantReason string
	}{
		{
			name:       "作動済みでも giveback が SL の外側なら SL が先に効く",
			mutate:     func(p *position.Position) { p.RatchetArmed = true; p.PeakUnrealizedJPY = 10890 },
			wantPrice:  37980,
			wantReason: "stop_loss",
		},
		{
			name:       "ピークが伸びて giveback 線が SL の内側に来たら ratchet が効く",
			mutate:     func(p *position.Position) { p.RatchetArmed = true; p.PeakUnrealizedJPY = peakInsideSL },
			wantPrice:  wantRatchetLine,
			wantReason: "ratchet_takeprofit",
		},
		{
			name:       "未作動なら giveback 脚は存在しない — SL だけ",
			mutate:     func(p *position.Position) { p.PeakUnrealizedJPY = 5780 },
			wantPrice:  37980,
			wantReason: "stop_loss",
		},
		{
			name:       "ratchet を持たない建玉は SL のまま",
			mutate:     func(p *position.Position) { p.RatchetArmJPY = 0; p.RatchetGivebackJPY = 0 },
			wantPrice:  37980,
			wantReason: "stop_loss",
		},
		{
			name:       "SL も ratchet も無い(外部採用建玉)なら守りは無い",
			mutate:     func(p *position.Position) { p.StopLossPrice = 0; p.RatchetArmJPY = 0 },
			wantPrice:  0,
			wantReason: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := trail285A
			tc.mutate(&p)
			gotPrice, gotReason := position.ProtectiveExit(p)
			if gotPrice != tc.wantPrice || gotReason != tc.wantReason {
				t.Fatalf("ProtectiveExit = (%v, %q), want (%v, %q)", gotPrice, gotReason, tc.wantPrice, tc.wantReason)
			}
		})
	}
}

// 売りは上下が逆 — 価格上昇で先に届くのは**低い方**。
func TestProtectiveExitSellSideMirrors(t *testing.T) {
	p := position.Position{
		Side: order.SideSell, Quantity: 100, EntryPrice: 1000,
		StopLossPrice: 1080, RatchetArmJPY: 20, RatchetGivebackJPY: 150,
		RatchetArmed: true, PeakUnrealizedJPY: 30,
	}
	// SELL: 含み u の価格は entry - u。giveback 線 1120 は SL 1080 より遠いので SL が先。
	if got, reason := position.ProtectiveExit(p); got != 1080 || reason != "stop_loss" {
		t.Fatalf("SL が先のはず: got (%v, %q)", got, reason)
	}
	p.PeakUnrealizedJPY = 200 // giveback 線 = 1000 - (200-150) = 950 → SL より内側
	if got, reason := position.ProtectiveExit(p); got != 950 || reason != "ratchet_takeprofit" {
		t.Fatalf("ratchet が先のはず: got (%v, %q)", got, reason)
	}
}

// 画面と engine が食い違わない担保: ProtectiveExit の値段ちょうどで EvaluateExit が同じ理由で決済する。
func TestProtectiveExitAgreesWithEvaluateExit(t *testing.T) {
	p := position.Position{
		Side: order.SideBuy, Quantity: 100, EntryPrice: 41050,
		StopLossPrice: 37980, RatchetArmJPY: 8757.857142857143,
		RatchetGivebackJPY: 21894.64285714286, RatchetArmed: true, PeakUnrealizedJPY: 10890,
	}
	price, reason := position.ProtectiveExit(p)
	d := position.EvaluateExit(p, price, p.OpenedAt)
	if !d.Exit || d.Reason != reason {
		t.Fatalf("EvaluateExit(%v) = %+v, want Exit=true Reason=%q", price, d, reason)
	}
}
