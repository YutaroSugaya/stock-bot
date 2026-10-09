package position

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
)

// 出口幾何は円/株。tick は発注価格を呼値グリッドに丸めるためだけに使う。
func TestUnrealizedJPYIsPerShareYen(t *testing.T) {
	buy := Position{Side: order.SideBuy, EntryPrice: 15890}
	if got := buy.UnrealizedJPY(15810); got != -80 {
		t.Errorf("BUY UnrealizedJPY = %v, want -80", got)
	}
	if got := buy.UnrealizedJPY(16390); got != 500 {
		t.Errorf("BUY UnrealizedJPY = %v, want +500", got)
	}
	sell := Position{Side: order.SideSell, EntryPrice: 15890}
	if got := sell.UnrealizedJPY(15810); got != 80 {
		t.Errorf("SELL UnrealizedJPY = %v, want +80 (売りは下落が順行)", got)
	}
	// 呼値は一切関与しない(TickSizeAtEntry が 0 でも動く)。
	if got := (Position{Side: order.SideBuy, EntryPrice: 100}).UnrealizedJPY(105); got != 5 {
		t.Errorf("呼値未設定で UnrealizedJPY = %v, want 5", got)
	}
}

// TP/SL は建玉時に確定した絶対価格で判定する(arm から entry まで時間が空くので config は円幅で持つ)。
func TestEvaluateExitUsesFrozenAbsolutePrices(t *testing.T) {
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	p := Position{
		Side: order.SideBuy, EntryPrice: 15890, Quantity: 100,
		TakeProfitJPY: 500, StopLossJPY: 250,
		TakeProfitPrice: 16390, StopLossPrice: 15640,
		OpenedAt: now,
	}
	if d := EvaluateExit(p, 15810, now); d.Exit {
		t.Fatalf("-80円/株 で決済されている(SL は -250円): %+v", d)
	}
	if d := EvaluateExit(p, 15640, now); !d.Exit || d.Reason != "stop_loss" {
		t.Fatalf("SL 到達で決済されない: %+v", d)
	}
	if d := EvaluateExit(p, 16390, now); !d.Exit || d.Reason != "take_profit" {
		t.Fatalf("TP 到達で決済されない: %+v", d)
	}
}

func TestEvaluateExitSellSideAbsolutePrices(t *testing.T) {
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	p := Position{
		Side: order.SideSell, EntryPrice: 3000, TakeProfitJPY: 100, StopLossJPY: 50,
		TakeProfitPrice: 2900, StopLossPrice: 3050, OpenedAt: now,
	}
	if d := EvaluateExit(p, 2900, now); !d.Exit || d.Reason != "take_profit" {
		t.Fatalf("売りの TP(下落)で決済されない: %+v", d)
	}
	if d := EvaluateExit(p, 3050, now); !d.Exit || d.Reason != "stop_loss" {
		t.Fatalf("売りの SL(上昇)で決済されない: %+v", d)
	}
}

// 絶対価格が凍結されていない建玉(外部採用など)では TP/SL を評価しない — 0 を「即決済」と読まない。
func TestEvaluateExitSkipsWhenPricesUnset(t *testing.T) {
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	p := Position{Side: order.SideBuy, EntryPrice: 3000, OpenedAt: now}
	if d := EvaluateExit(p, 1, now); d.Exit {
		t.Fatalf("TP/SL 未設定で決済された: %+v", d)
	}
}

// ratchet も円/株。到達ピークからの戻し幅で判定する。
func TestEvaluateExitRatchetInJPY(t *testing.T) {
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	p := Position{
		Side: order.SideBuy, EntryPrice: 3000, OpenedAt: now,
		RatchetArmJPY: 100, RatchetGivebackJPY: 40,
		PeakUnrealizedJPY: 120, RatchetArmed: true,
		StopLossJPY: 250, StopLossPrice: 2750,
	}
	if d := EvaluateExit(p, 3090, now); d.Exit { // 戻し 30円 < 40円
		t.Fatalf("戻し 30円 で決済された: %+v", d)
	}
	if d := EvaluateExit(p, 3075, now); !d.Exit || d.Reason != "ratchet_takeprofit" {
		t.Fatalf("戻し 45円 で決済されない: %+v", d)
	}
}

// 建値からの円幅 → 絶対価格。呼値グリッドへの丸めはここで1回だけ効く。
func TestTPSLPricesFromJPYWidth(t *testing.T) {
	// 7735 は TOPIX500・15,890円 → 呼値 5円。500/250 はちょうど乗る。
	tp, sl := TPSLPricesFromJPY("7735", order.SideBuy, 15890, 500, 250)
	if tp != 16390 || sl != 15640 {
		t.Fatalf("BUY tp/sl = %v/%v, want 16390/15640", tp, sl)
	}
	// 呼値に乗らない幅は丸める(broker が刻み違反を弾くため)。
	tp, sl = TPSLPricesFromJPY("7735", order.SideBuy, 15890, 502, 253)
	if tp != 16390 || sl != 15635 {
		t.Fatalf("丸め後 tp/sl = %v/%v, want 16390/15635", tp, sl)
	}
	// 売りは上下が逆。
	tp, sl = TPSLPricesFromJPY("7735", order.SideSell, 15890, 500, 250)
	if tp != 15390 || sl != 16140 {
		t.Fatalf("SELL tp/sl = %v/%v, want 15390/16140", tp, sl)
	}
	// 幅 0 = 「無し」→ 価格も 0。
	if tp, sl := TPSLPricesFromJPY("7735", order.SideBuy, 15890, 0, 0); tp != 0 || sl != 0 {
		t.Fatalf("幅0 で tp/sl = %v/%v, want 0/0", tp, sl)
	}
}
