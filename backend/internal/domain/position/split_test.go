package position

import (
	"math"
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
)

func splitFixture() Position {
	return Position{
		ID: 24, Symbol: "6368", Side: order.SideBuy, Quantity: 100, EntryPrice: 11570,
		TakeProfitJPY: 900, StopLossJPY: 1000, TakeProfitPrice: 12470, StopLossPrice: 10570,
		ExtensionUnrealizedJPY: 50, EarlyExitTargetJPY: 40,
		RatchetArmJPY: 300, RatchetGivebackJPY: 450,
		PeakUnrealizedJPY: 1100, TroughUnrealizedJPY: -200, RatchetArmed: true,
		EntryFeeJPY: 0, MaxHoldMinutes: 14400, Status: StatusOpen,
	}
}

// 1:5 分割(2026-09-29 の 6368)。建玉の**経済的な中身は 1 円も変えない**:
// 株数 × 建値(建玉金額)と、株数 × 円/株 の幅(TP/SL までの損益)が不変であること。
func TestSplitAdjusted_OneToFivePreservesEconomics(t *testing.T) {
	p := splitFixture()
	day := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	got, err := SplitAdjusted(p, 5, day)
	if err != nil {
		t.Fatal(err)
	}
	if got.Quantity != 500 {
		t.Fatalf("株数 = %d, want 500", got.Quantity)
	}
	near := func(name string, a, b float64) {
		t.Helper()
		if math.Abs(a-b) > 1e-9 {
			t.Fatalf("%s = %v, want %v", name, a, b)
		}
	}
	near("建値", got.EntryPrice, 2314)
	near("TP 価格", got.TakeProfitPrice, 2494)
	near("SL 価格", got.StopLossPrice, 2114)
	near("建玉金額", float64(got.Quantity)*got.EntryPrice, float64(p.Quantity)*p.EntryPrice)
	near("SL までの損失", float64(got.Quantity)*got.StopLossJPY, float64(p.Quantity)*p.StopLossJPY)
	near("TP までの利益", float64(got.Quantity)*got.TakeProfitJPY, float64(p.Quantity)*p.TakeProfitJPY)
	near("ratchet arm", float64(got.Quantity)*got.RatchetArmJPY, float64(p.Quantity)*p.RatchetArmJPY)
	near("ratchet giveback", float64(got.Quantity)*got.RatchetGivebackJPY, float64(p.Quantity)*p.RatchetGivebackJPY)
	near("peak", float64(got.Quantity)*got.PeakUnrealizedJPY, float64(p.Quantity)*p.PeakUnrealizedJPY)
	near("trough", float64(got.Quantity)*got.TroughUnrealizedJPY, float64(p.Quantity)*p.TroughUnrealizedJPY)
	near("延長の含み", float64(got.Quantity)*got.ExtensionUnrealizedJPY, float64(p.Quantity)*p.ExtensionUnrealizedJPY)
	near("早期利確", float64(got.Quantity)*got.EarlyExitTargetJPY, float64(p.Quantity)*p.EarlyExitTargetJPY)
	near("分割係数", got.SplitFactor, 5)
	if !got.SplitAdjustedOn.Equal(day) {
		t.Fatalf("調整日 = %v, want %v", got.SplitAdjustedOn, day)
	}
	if !got.RatchetArmed || got.MaxHoldMinutes != p.MaxHoldMinutes || got.ID != p.ID {
		t.Fatal("分割と無関係な凍結値(arm 済み・保有期限・id)を変えた")
	}
}

// 分割調整した建玉は、分割後の値段で**分割前と同じ判定**を返す(権利落ちの寄りで SL を割らない)。
func TestSplitAdjusted_ExitDecisionMatchesPreSplit(t *testing.T) {
	p := splitFixture()
	p.RatchetArmJPY, p.RatchetGivebackJPY, p.RatchetArmed, p.PeakUnrealizedJPY = 0, 0, false, 0
	p.TakeProfitJPY, p.TakeProfitPrice = 1900, 13470 // 前日終値 12,595 では未達の TP
	now := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	p.OpenedAt = now.Add(-24 * time.Hour)
	if d := EvaluateExit(p, 2495, now); !d.Exit {
		t.Fatal("前提: 調整前は権利落ちの寄り 2,495 円で SL を割ったと誤判定するはず")
	}
	adj, err := SplitAdjusted(p, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	if d := EvaluateExit(adj, 2495, now); d.Exit {
		t.Fatalf("調整後も決済した(理由 %s)— 分割は損切りではない", d.Reason)
	}
	// 分割前 10,500 円(SL 10,570 割れ)= 分割後 2,100 円 は、調整後も SL。
	if d := EvaluateExit(adj, 2100, now); !d.Exit || d.Reason != "stop_loss" {
		t.Fatalf("分割後 2,100 円で SL が効かない: %+v", d)
	}
}

// 累積: 2 回目の分割は係数を掛け合わせる。
func TestSplitAdjusted_CumulativeFactor(t *testing.T) {
	p := splitFixture()
	d1 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	a, _ := SplitAdjusted(p, 2, d1)
	b, err := SplitAdjusted(a, 2, d2)
	if err != nil {
		t.Fatal(err)
	}
	if b.Quantity != 400 || math.Abs(b.SplitFactor-4) > 1e-9 || math.Abs(b.EntryPrice-11570.0/4) > 1e-9 {
		t.Fatalf("累積 = qty %d factor %v entry %v", b.Quantity, b.SplitFactor, b.EntryPrice)
	}
}

// 🛑 株数が整数にならない分割は**調整しない**(error)。端数の扱いは broker の権利処理次第で、
// 推測で株数を作ると台帳と broker の建玉がずれる。
func TestSplitAdjusted_RejectsFractionalShares(t *testing.T) {
	p := splitFixture()
	p.Quantity = 1
	if _, err := SplitAdjusted(p, 1.5, time.Time{}); err == nil {
		t.Fatal("1 株 × 1.5 = 1.5 株を受け入れた")
	}
	p.Quantity = 5
	if _, err := SplitAdjusted(p, 0.1, time.Time{}); err == nil {
		t.Fatal("5 株 × 0.1(10:1 併合)= 0.5 株を受け入れた")
	}
}

func TestSplitAdjusted_RejectsInvalidFactor(t *testing.T) {
	for _, r := range []float64{0, -2, 1, math.NaN(), math.Inf(1)} {
		if _, err := SplitAdjusted(splitFixture(), r, time.Time{}); err == nil {
			t.Fatalf("不正な倍率 %v を受け入れた", r)
		}
	}
}

// 併合 10:1: 100 株 → 10 株・建値 ×10。
func TestSplitAdjusted_ReverseSplit(t *testing.T) {
	got, err := SplitAdjusted(splitFixture(), 0.1, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Quantity != 10 || math.Abs(got.EntryPrice-115700) > 1e-6 {
		t.Fatalf("併合 = qty %d entry %v", got.Quantity, got.EntryPrice)
	}
}

// 🚨 板へ送る TP/SL は呼値の格子に乗せる。2,659 ÷ 5 = 531.8 円のまま live の守りに使うと、
// 立花が「逆指値条件に誤りがあります」で注文ごと拒否し、建玉が裸で残る(9501 と同じ形)。
func TestSplitAdjusted_ProtectivePricesOnTickGrid(t *testing.T) {
	p := splitFixture()
	p.Symbol = "0000" // 呼値テーブル外 = 粗い刻み(500〜3,000 円帯は 1 円)
	p.EntryPrice, p.StopLossPrice, p.TakeProfitPrice = 2871, 2659, 3103
	got, err := SplitAdjusted(p, 5, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got.StopLossPrice != 532 || got.TakeProfitPrice != 621 {
		t.Fatalf("格子外の守り: SL %v TP %v, want 532 / 621", got.StopLossPrice, got.TakeProfitPrice)
	}
	if got.EntryPrice != 574.2 {
		t.Fatalf("建値は丸めない(VWAP と同じく格子外でよい): %v", got.EntryPrice)
	}
}
