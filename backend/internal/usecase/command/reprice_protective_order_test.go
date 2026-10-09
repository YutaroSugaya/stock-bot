package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/port"
)

// 🚨 **守りの値段を変える**(取消 → 再発注・場中実行)。
//
// 既存の ReplaceProtectiveOrder は「板の値段を引き継いで期日だけ延ばす」ので、値段を
// 変える手段が無かった。こちらは**人間が指定した値段**で置き直す。
//
// 🛑 **場中でも撃つ。**取消と再発注の間は守りが消えるが、それは人間が承知の上で
// 選ぶ経路。だから代わりに **取消の前に検証を全部済ませる** ——
// 建玉の特定・口座区分・値段の向き。過去の事故は「取消してから再発注が
// 組めないと分かった」形だった。

var repriceNow = time.Date(2026, 8, 26, 10, 30, 0, 0, clock.JST) // 場中

func repriceFixture(t *testing.T) (*repository.InMemoryPositionRepo, *fakeReplaceBroker) {
	t.Helper()
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4901", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{
			"4901": {guardAtCeiling()},
		}},
		requireExec: order.ExecMarginSystem,
	}
	return repo, brk
}

func newReprice(repo *repository.InMemoryPositionRepo, brk *fakeReplaceBroker, trip *fakeTripper) *RepriceProtectiveOrder {
	return NewRepriceProtectiveOrder(repo, brk, replaceHours(), func() time.Time { return repriceNow }, trip)
}

// 🛑 場中でも撃つ(この経路の存在意義)。値段・区分・建玉 ID が正しく載ること。
func TestRepriceProtectiveOrder_CancelsThenPlacesAtTheGivenPrices(t *testing.T) {
	repo, brk := repriceFixture(t)
	r := newReprice(repo, brk, &fakeTripper{})

	id, err := r.Execute(context.Background(), RepriceProtectiveInput{
		Symbol: "4901", TakeProfit: 3505, StopLoss: 3115})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if id == "" {
		t.Fatal("新しい注文 ID が空")
	}
	if len(brk.cancelled) != 1 || brk.cancelled[0] != "13014332" {
		t.Fatalf("cancelled = %v — 既存の守りを取り消していない", brk.cancelled)
	}
	got := brk.placed[0]
	// 多日建玉の板の守りは **stop-only**(TP は板に載せない)。SL は指定どおり。
	if got.TakeProfit != 0 || got.StopLoss != 3115 {
		t.Fatalf("TP/SL = %v/%v — 多日は TP 0(stop-only)/ SL は指定の 3115 のはず", got.TakeProfit, got.StopLoss)
	}
	if got.ExecKind != order.ExecMarginSystem || got.BrokerPositionID != "b-4901" {
		t.Fatalf("区分/建玉ID が台帳から来ていない: %+v", got)
	}
	if got.Quantity != 100 {
		t.Fatalf("Quantity = %d — 台帳の数量でない", got.Quantity)
	}
	// 期日は 9 営業日先を張り直す(取消 → 再発注なので天井もリセットされる)。
	want := replaceHours().NthTradingDayFrom(repriceNow, settleMaxTradingDays)
	if !got.ExpireOn.Equal(want) {
		t.Fatalf("ExpireOn = %v, want %v", got.ExpireOn, want)
	}
}

// 🚨 **TP と SL を取り違えたら取り消さない。**買い建玉の守りは TP が上・SL が下。
// 逆に入れると「利確が現値より下」= 即時成行のような注文になり、取消済みなら
// 建玉を意図せず投げ捨てる。人間が数字を入れる経路なので、ここは必ず落とす。
func TestRepriceProtectiveOrder_RejectsTransposedPrices(t *testing.T) {
	repo, brk := repriceFixture(t)
	r := newReprice(repo, brk, &fakeTripper{})

	_, err := r.Execute(context.Background(), RepriceProtectiveInput{
		Symbol: "4901", TakeProfit: 3115, StopLoss: 3505}) // 逆
	if err == nil {
		t.Fatal("TP < SL(買い建玉)を通した")
	}
	if len(brk.cancelled) != 0 {
		t.Fatalf("cancelled = %v — 値段が不正なのに取り消した(裸になる)", brk.cancelled)
	}
	if !strings.Contains(err.Error(), "取り消さない") {
		t.Fatalf("err = %v", err)
	}
}

// 🛑 SL 無しは守りではない。
func TestRepriceProtectiveOrder_RequiresAStopLoss(t *testing.T) {
	repo, brk := repriceFixture(t)
	r := newReprice(repo, brk, &fakeTripper{})

	if _, err := r.Execute(context.Background(), RepriceProtectiveInput{Symbol: "4901", TakeProfit: 3505}); err == nil {
		t.Fatal("SL 無しを通した")
	}
	if len(brk.cancelled) != 0 {
		t.Fatal("SL 無しなのに取り消した")
	}
}

// 🛑 板に守りが無いなら**取り消すものが無い**。黙って新規に置くと、照会が
// 取りこぼしただけの場合に守りが二重になる。arm を使えと言って止まる。
func TestRepriceProtectiveOrder_RefusesWhenNoGuardIsResting(t *testing.T) {
	repo, brk := repriceFixture(t)
	brk.orders = map[string][]port.ProtectiveOrderInfo{}
	r := newReprice(repo, brk, &fakeTripper{})

	_, err := r.Execute(context.Background(), RepriceProtectiveInput{
		Symbol: "4901", TakeProfit: 3505, StopLoss: 3115})
	if err == nil || !strings.Contains(err.Error(), "arm") {
		t.Fatalf("err = %v — 守りが無いことを arm の案内付きで返していない", err)
	}
	if len(brk.placed) != 0 {
		t.Fatal("守りが無いのに置いた(二重になりうる)")
	}
}

// 🛑 取消が失敗 = 守りは板に残っている。**現状維持が正しい**ので trip しない。
func TestRepriceProtectiveOrder_DoesNotTripWhenCancelFails(t *testing.T) {
	repo, brk := repriceFixture(t)
	brk.cancelErr = map[string]error{"13014332": errors.New("取消できません")}
	trip := &fakeTripper{}
	r := newReprice(repo, brk, trip)

	if _, err := r.Execute(context.Background(), RepriceProtectiveInput{
		Symbol: "4901", TakeProfit: 3505, StopLoss: 3115}); err == nil {
		t.Fatal("取消の失敗が握り潰された")
	}
	if len(trip.reasons) != 0 {
		t.Fatalf("trip = %v — 守りは板に残っているのに緊急停止した", trip.reasons)
	}
	if len(brk.placed) != 0 {
		t.Fatal("取消が失敗したのに置いた(守りが二重になる)")
	}
}

// 🚨 取消は通ったのに置けない = **建玉がいま裸**。emergency を trip して叫ぶ。
func TestRepriceProtectiveOrder_TripsWhenRePlaceFails(t *testing.T) {
	repo, brk := repriceFixture(t)
	brk.placeErr = errors.New("注文が拒否されました")
	trip := &fakeTripper{}
	r := newReprice(repo, brk, trip)

	_, err := r.Execute(context.Background(), RepriceProtectiveInput{
		Symbol: "4901", TakeProfit: 3505, StopLoss: 3115})
	if err == nil {
		t.Fatal("再発注の失敗が握り潰された")
	}
	if len(trip.reasons) != 1 || !strings.Contains(trip.reasons[0], "4901") {
		t.Fatalf("trip = %v — 裸なのに緊急停止していない", trip.reasons)
	}
	// 🛑 復旧手段を文言に含める(裸のまま人間が何をすべきか分からない状態にしない)。
	if !strings.Contains(err.Error(), "arm") {
		t.Fatalf("err = %v — 復旧コマンドが案内されていない", err)
	}
}

// 🚨 **取り消す前に分かることは全部取り消す前に確かめる** — 呼値と値幅制限もその一つ。
// adapter は値段を丸めずに送るので、格子外の値段は取消の
// **後**で立花に拒否され、建玉が裸のまま emergency を trip していた。
func TestRepriceProtectiveOrder_RejectsOffGridPricesBeforeCancelling(t *testing.T) {
	repo, brk := repriceFixture(t)
	r := newReprice(repo, brk, &fakeTripper{})
	off := 3115 + market.TickSizeOf("4901", 3115)/2 // 半呼値ずらし = 格子の外

	if _, err := r.Execute(context.Background(), RepriceProtectiveInput{
		Symbol: "4901", TakeProfit: 3505, StopLoss: off}); err == nil {
		t.Fatal("格子外の SL を受理した")
	}
	if len(brk.cancelled) != 0 || len(brk.placed) != 0 {
		t.Fatalf("格子外の値段で取消/再発注に進んだ: cancelled=%v placed=%d", brk.cancelled, len(brk.placed))
	}
}

// 🚨 SL が今日の値幅制限の外なら**取り消さない**(置きに行っても注文ごと拒否 = 裸)。
func TestRepriceProtectiveOrder_RejectsStopOutsideLimitBandBeforeCancelling(t *testing.T) {
	repo, brk := repriceFixture(t)
	trip := &fakeTripper{}
	const ref = 4000.0
	_, down, ok := risk.LimitBandFor(ref)
	if !ok {
		t.Fatal("precondition: 基準値段 4000 の帯が引けない")
	}
	sl := market.RoundToTickOf("4901", down-market.TickSizeOf("4901", down)*2) // 帯の下限より外
	r := newReprice(repo, brk, trip).
		WithPriceLimitRef(func(context.Context, string) float64 { return ref })

	if _, err := r.Execute(context.Background(), RepriceProtectiveInput{
		Symbol: "4901", TakeProfit: 4200, StopLoss: sl}); err == nil {
		t.Fatalf("帯の外の SL %v を受理した", sl)
	}
	if len(brk.cancelled) != 0 || len(brk.placed) != 0 {
		t.Fatalf("帯の外の SL で取消/再発注に進んだ: cancelled=%v placed=%d", brk.cancelled, len(brk.placed))
	}
	if trip.Active() {
		t.Fatalf("板の守りは無傷なのに trip した: %v", trip.reasons)
	}
}

// 🛑 TP は**落とすだけ**(SL は置く)。CLAUDE.md「TP が帯の外なら TP 脚だけ落として SL を置く」。
func TestRepriceProtectiveOrder_DropsTakeProfitOutsideLimitBand(t *testing.T) {
	repo, brk := repriceFixture(t)
	const ref = 2803.0 // 帯 2,303〜3,303(前日終値が下がって凍結 TP が帯の外に出る形)
	up, _, _ := risk.LimitBandFor(ref)
	tp := market.RoundToTickOf("4901", up+market.TickSizeOf("4901", up)*10) // 帯の上限より外
	sl := market.RoundToTickOf("4901", 2986.5)
	r := newReprice(repo, brk, &fakeTripper{}).
		WithPriceLimitRef(func(context.Context, string) float64 { return ref })

	if _, err := r.Execute(context.Background(), RepriceProtectiveInput{
		Symbol: "4901", TakeProfit: tp, StopLoss: sl}); err != nil {
		t.Fatalf("TP が帯の外なだけで失敗した: %v", err)
	}
	if len(brk.placed) != 1 {
		t.Fatalf("再発注 %d 件", len(brk.placed))
	}
	if got := brk.placed[0]; got.TakeProfit != 0 || got.StopLoss != sl {
		t.Fatalf("TP=%v SL=%v — TP 脚だけ 0(stop-only)に落として SL は置くはず", got.TakeProfit, got.StopLoss)
	}
}
