package protectiveboard

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 🚨 **画面の TP/SL が台帳の凍結値で、板の実体ではなかった**。
//
// 人間が証券アプリで TP/SL を締めることがある。
// 台帳の凍結値は**建てたときの値**なので、締めた後の画面は**実際には起こらない
// 値段**を「broker の OCO に入っているのと同じ値段」として出し続けていた。
// 実弾の出口を画面から読めない状態は、守りが消えていても気づけない状態でもある。

func boardOrder(sym string, tp, sl float64) port.ProtectiveOrderInfo {
	return port.ProtectiveOrderInfo{
		OrderID: "o-" + sym, Symbol: sym, HasStopLeg: true,
		Side: order.SideSell, Quantity: 100,
		LimitPrice: tp, StopTrigger: sl,
		ExpireOn: time.Date(2026, 9, 7, 0, 0, 0, 0, clock.JST),
	}
}

var boardAt = time.Date(2026, 8, 25, 9, 45, 0, 0, clock.JST)

// 🛑 **一度も読んでいない**を「守りが無い」と読ませない。起動直後や照会失敗で
// 空になったスナップショットを裸と読むと、毎朝 3 件の偽の警報が出て麻痺する。
func TestProtectiveBoardDistinguishesNeverFetchedFromUnguarded(t *testing.T) {
	var b State
	v := b.View()
	if v["fetched"] != false {
		t.Fatalf("fetched=%v, want false", v["fetched"])
	}
	if _, ok := v["unguarded"]; ok {
		t.Fatal("一度も読んでいないのに unguarded を出した — 偽の裸警報になる")
	}
}

func TestProtectiveBoardExposesTheBoardPrices(t *testing.T) {
	var b State
	b.set(boardAt, map[string]port.ProtectiveOrderInfo{
		"4704": boardOrder("4704", 5611, 5471),
	}, []string{}, nil, nil, nil)

	v := b.View()
	if v["fetched"] != true {
		t.Fatalf("fetched=%v, want true", v["fetched"])
	}
	by, _ := v["by_symbol"].(map[string]any)
	g, _ := by["4704"].(map[string]any)
	if g == nil {
		t.Fatalf("by_symbol に 4704 が無い: %v", v)
	}
	if g["take_profit"] != 5611.0 || g["stop_loss"] != 5471.0 {
		t.Fatalf("板の値段が出ていない: %v", g)
	}
	if g["expire_on"] != "2026-09-07" {
		t.Fatalf("expire_on=%v — 期日が読めないと切れる日が画面から分からない", g["expire_on"])
	}
	// 🛑 いつ時点の板かを必ず出す。古いスナップショットを「今の実体」と読ませない。
	if v["fetched_at"] == "" || v["fetched_at"] == nil {
		t.Fatal("fetched_at が無い — 古い写しを現在の実体と読める")
	}
}

// 🚨 **多日建玉なのに板に守りが無い = 裸**。live では 46 分間、画面は
// 凍結値の TP/SL を平常どおり出していて、裸であることがどこにも出ていなかった。
func TestProtectiveBoardFlagsUnguardedPositions(t *testing.T) {
	var b State
	b.set(boardAt, map[string]port.ProtectiveOrderInfo{}, []string{"4751", "4901"}, nil, nil, nil)

	v := b.View()
	un, _ := v["unguarded"].([]string)
	if len(un) != 2 || un[0] != "4751" {
		t.Fatalf("unguarded=%v, want [4751 4901] — 裸の建玉が画面に出ない", v["unguarded"])
	}
}

// 🛑 照会が落ちた回で**直前の写しを消さない**。消すと「守りが無い」と区別できない
// 空の画面になり、照会障害が裸の警報に化ける(またはその逆)。
func TestProtectiveBoardKeepsTheLastSnapshotOnFetchError(t *testing.T) {
	var b State
	b.set(boardAt, map[string]port.ProtectiveOrderInfo{"4704": boardOrder("4704", 5611, 5471)}, []string{}, nil, nil, nil)
	b.setErr(boardAt.Add(15*time.Minute), "tachibana OrderList: result=10006")

	v := b.View()
	by, _ := v["by_symbol"].(map[string]any)
	if _, ok := by["4704"]; !ok {
		t.Fatal("照会が落ちた回で直前の写しを消した — 障害が『守りが無い』に化ける")
	}
	if v["error"] == "" || v["error"] == nil {
		t.Fatal("照会の失敗が画面に出ない")
	}
	if v["fetched_at"] != "2026-08-25 09:45:00" {
		t.Fatalf("fetched_at=%v — 失敗した回の時刻で上書きしている(写しは 09:45 のもの)", v["fetched_at"])
	}
}

// 🚨 **昼休みに穴が開いていた**。
//
