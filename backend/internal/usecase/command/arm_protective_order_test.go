package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
)

// 🚨 **守りが 1 本も無い多日建玉に守りを置く**経路。
//
// 置き直し(取消 → 再発注)の再発注が口座区分の取り違えで拒否されると、
// 取消済みの live の建玉が寄り前に裸で残る。そこで判明したのは —— bot には
// **守りを置く口が新規建て時にしか無い**ということ。板に守りが無い状態から
// 復旧する手段がコードに存在せず、人間がアプリで手打ちするしかなかった。
//
// 🛑 **これは「守りを置く」だけの経路。**建玉を作らない・増やさない・決済しない。
// リスクは単調に減る方向にしか動かない。だから emergency 中でも撃てる
// (emergency は新規建てを止めるもので、守りを置くのを止めるものではない)。

func armHours() session.TradingHours { return replaceHours() }

func armedRepo(t *testing.T) *repository.InMemoryPositionRepo {
	t.Helper()
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4704", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	return repo
}

func newArm(repo *repository.InMemoryPositionRepo, brk *fakeReplaceBroker, now time.Time) *ArmProtectiveOrder {
	return NewArmProtectiveOrder(repo, brk, armHours(), func() time.Time { return now })
}

var armNow = time.Date(2026, 8, 25, 8, 30, 0, 0, clock.JST)

// 🛑 **人間が渡すのは値段だけ。**数量・side・口座区分・建玉 ID は台帳から取る。
// 人間に数量を打たせると、打ち間違いがそのまま「建玉より多い返済注文」になる。
func TestArmProtectiveOrder_PlacesFromTheLedgerAndTakesOnlyPricesFromTheHuman(t *testing.T) {
	repo := armedRepo(t)
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{}},
		requireExec:      order.ExecMarginSystem,
	}

	id, err := newArm(repo, brk, armNow).Execute(context.Background(),
		ArmProtectiveInput{Symbol: "4704", TakeProfit: 5611, StopLoss: 5471})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if id == "" {
		t.Fatal("注文 ID が空 — 置けたのか確認できない")
	}
	if len(brk.placed) != 1 {
		t.Fatalf("placed = %d, want 1", len(brk.placed))
	}
	got := brk.placed[0]
	if got.Side != order.SideSell { // 建玉は BUY なので守りは SELL
		t.Fatalf("Side = %v, want SELL", got.Side)
	}
	if got.Quantity != 100 {
		t.Fatalf("Quantity = %d, want 100(台帳の数量)", got.Quantity)
	}
	if got.ExecKind != order.ExecMarginSystem {
		t.Fatalf("ExecKind = %q — 台帳の凍結区分を引き継いでいない", got.ExecKind)
	}
	if got.BrokerPositionID != "b-4704" {
		t.Fatalf("BrokerPositionID = %q, want b-4704", got.BrokerPositionID)
	}
	// 多日建玉の板の守りは **stop-only**。人間が TP を渡しても板には載せない
	// (翌朝の繰越で帯の外に出ると SL ごと失効する)。SL は指定どおり。
	if got.TakeProfit != 0 || got.StopLoss != 5471 {
		t.Fatalf("TP/SL = %v/%v — 多日は TP 0(stop-only)/ SL は人間の指定 5471 のはず", got.TakeProfit, got.StopLoss)
	}
	// 🛑 期日は**呼び手が営業日で数える**。当日限りにすると翌日から裸に戻る。
	want := armHours().NthTradingDayFrom(armNow, settleMaxTradingDays)
	if !got.ExpireOn.Equal(want) {
		t.Fatalf("ExpireOn = %v, want %v", got.ExpireOn, want)
	}
}

// 🛑 **守りを二重に置かない。**返済注文が建玉数量を超えると broker は両方弾きうる
// (拘束数量の超過)。「もう1本置いておけば安心」は守りの経路では成立しない。
func TestArmProtectiveOrder_RefusesWhenAGuardIsAlreadyResting(t *testing.T) {
	repo := armedRepo(t)
	brk := &fakeReplaceBroker{fakeExpiryBroker: fakeExpiryBroker{
		orders: map[string][]port.ProtectiveOrderInfo{
			"4704": {{OrderID: "already", Symbol: "4704", HasStopLeg: true,
				Side: order.SideSell, Quantity: 100}},
		},
	}}

	_, err := newArm(repo, brk, armNow).Execute(context.Background(),
		ArmProtectiveInput{Symbol: "4704", TakeProfit: 5611, StopLoss: 5471})
	if err == nil {
		t.Fatal("既に守りがあるのに置いた — 返済可能数量を超えて両方弾かれうる")
	}
	if len(brk.placed) != 0 {
		t.Fatalf("placed = %v — 二重に置いた", brk.placed)
	}
	if !strings.Contains(err.Error(), "already") {
		t.Fatalf("err = %v — どの注文と衝突したのか分からない", err)
	}
}

// 🛑 SL の無い「守り」は守りではない。TP だけ置くと下方向は裸のまま。
func TestArmProtectiveOrder_RequiresAStopLoss(t *testing.T) {
	repo := armedRepo(t)
	brk := &fakeReplaceBroker{fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{}}}

	if _, err := newArm(repo, brk, armNow).Execute(context.Background(),
		ArmProtectiveInput{Symbol: "4704", TakeProfit: 5611}); err == nil {
		t.Fatal("SL 無しで置いた — 下方向が裸のままの『守り』")
	}
	if len(brk.placed) != 0 {
		t.Fatal("SL 無しの注文を送った")
	}
}

// 🛑 台帳に無い建玉には置かない。人間の打ち間違いで**他人の銘柄に返済注文**を
// 出すと、持っていない建玉の返済 = 実質の新規売りになりうる。
func TestArmProtectiveOrder_RefusesAnUnknownSymbol(t *testing.T) {
	repo := armedRepo(t)
	brk := &fakeReplaceBroker{fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{}}}

	if _, err := newArm(repo, brk, armNow).Execute(context.Background(),
		ArmProtectiveInput{Symbol: "9999", StopLoss: 100}); err == nil {
		t.Fatal("台帳に無い銘柄に守りを置いた")
	}
	if len(brk.placed) != 0 {
		t.Fatal("台帳に無い銘柄へ注文を送った")
	}
}

// 🛑 external(人間がアプリで建てた)には触らない。bot の台帳が数量も区分も
// 知らないので、返済注文を組む根拠が無い。
func TestArmProtectiveOrder_RefusesExternalPositions(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	if _, err := repo.Insert(context.Background(), port.PositionInsertInput{
		BrokerPositionID: "ext", Symbol: "4704", Side: order.SideBuy, Quantity: 100,
		OpenedAt: armNow.AddDate(0, 0, -3), HoldingMode: order.HoldingMultiday,
		MaxHoldMinutes: 14400, ExecKind: order.ExecMarginSystem, Source: position.SourceExternal,
	}); err != nil {
		t.Fatal(err)
	}
	brk := &fakeReplaceBroker{fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{}}}

	if _, err := newArm(repo, brk, armNow).Execute(context.Background(),
		ArmProtectiveInput{Symbol: "4704", StopLoss: 5471}); err == nil {
		t.Fatal("external 建玉に守りを置いた")
	}
	if len(brk.placed) != 0 {
		t.Fatal("external 建玉へ注文を送った")
	}
}

// 🛑 休場カレンダーが尽きたら**日付を捏造しない**。捏造した休場日を指定すると
// 注文ごと拒否される(= 置けたつもりで裸のまま)。
func TestArmProtectiveOrder_RefusesWhenTheCalendarIsExhausted(t *testing.T) {
	repo := armedRepo(t)
	brk := &fakeReplaceBroker{fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{}}}
	expired := armHours()
	expired.CalendarThrough = time.Date(2026, 8, 26, 0, 0, 0, 0, clock.JST)

	a := NewArmProtectiveOrder(repo, brk, expired, func() time.Time { return armNow })
	_, err := a.Execute(context.Background(), ArmProtectiveInput{Symbol: "4704", StopLoss: 5471})
	if err == nil {
		t.Fatal("カレンダー切れで日付を捏造した")
	}
	if !strings.Contains(err.Error(), "休場カレンダー") {
		t.Fatalf("err = %v — カレンダー切れではなく別経路で error になっている", err)
	}
	if len(brk.placed) != 0 {
		t.Fatal("捏造した日付で注文を送った")
	}
}

// 🚨 **live で踏んだ穴。** 多日建玉(例: 建値 2,935・凍結 TP 3,345 / SL 2,597)の守りが
// 前日に「繰越失効」で板から消え、裸のまま朝を迎えた。復旧の arm を叩いたら
// **`当該銘柄の値幅制限内の単価を入力してください` で拒否**され、建玉は裸のまま残った。
//
// 原因は TP 脚。前日終値が 2,908 → 2,803 に下がり、帯(±500)が 2,408〜3,408 から
// 2,303〜3,303 へずれて、**TP 3,345 が帯の外に出た**。立花は脚が 1 本でも帯の外だと
// **注文ごと**拒否するので、SL まで一緒に置けない。
//
// CLAUDE.md は「TP が帯の外なら **TP 脚だけ落として SL を置く**」と規定していて、
// `ExecuteOrder`(新規建て)はそのゲートを通っている。ところが **arm は通っていなかった** ——
// **裸を直す唯一の経路が、裸になった理由と同じ理由で落ちる**構造だった。
func TestArmProtectiveOrder_DropsTPLegWhenOutsideTodaysPriceLimit(t *testing.T) {
	repo := armedRepo(t)
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{}},
		requireExec:      order.ExecMarginSystem,
	}
	// 前日終値 2,803 → 帯は 2,303〜3,303。TP 3,345 は外、SL 2,597 は内。
	arm := newArm(repo, brk, armNow).
		WithPriceLimitRef(func(context.Context, string) float64 { return 2803 })

	if _, err := arm.Execute(context.Background(),
		ArmProtectiveInput{Symbol: "4704", TakeProfit: 3345, StopLoss: 2597}); err != nil {
		t.Fatalf("TP を落として SL だけでも置けるはず: %v", err)
	}
	if len(brk.placed) != 1 {
		t.Fatalf("発注 %d 件 — 1 件のはず", len(brk.placed))
	}
	if got := brk.placed[0].TakeProfit; got != 0 {
		t.Errorf("TP 脚 = %v — 帯の外なので 0(stop-only)に落とすはず。"+
			"落とさないと立花が注文ごと拒否し、建玉は裸のまま残る", got)
	}
	if got := brk.placed[0].StopLoss; got != 2597 {
		t.Errorf("SL = %v, want 2597 — SL は帯の内側なのでそのまま置く", got)
	}
}

// 🛑 TP が帯の内側なら**何も変えない**。「帯の外かもしれない」で勝手に落とすと、
// 板に置けたはずの利確を機会損失で捨てることになる。
func TestArmProtectiveOrder_MultidayIsStopOnlyEvenInsideTodaysPriceLimit(t *testing.T) {
	repo := armedRepo(t)
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{}},
		requireExec:      order.ExecMarginSystem,
	}
	// 前日終値 2,908 → 帯は 2,408〜3,408。TP 3,345 は内側。
	arm := newArm(repo, brk, armNow).
		WithPriceLimitRef(func(context.Context, string) float64 { return 2908 })

	if _, err := arm.Execute(context.Background(),
		ArmProtectiveInput{Symbol: "4704", TakeProfit: 3345, StopLoss: 2597}); err != nil {
		t.Fatalf("err = %v", err)
	}
	// 帯の内側でも **多日は TP を板に載せない**(stop-only)。翌朝の繰越で
	// 帯の外に出ると SL ごと失効するため(7220 / 6841)。SL は指定どおり置く。
	if got := brk.placed[0]; got.TakeProfit != 0 || got.StopLoss != 2597 {
		t.Errorf("TP/SL = %v/%v, want 0 / 2597 — 多日は帯の内側でも stop-only", got.TakeProfit, got.StopLoss)
	}
}

// 🚨 **SL が帯の外なら置きに行かない。** 行っても立花が注文ごと拒否するので結果は同じ
// 「裸のまま」だが、**broker の文言ではなく帯の数字を人間に返す**ことが要る ——
// 実際には拒否の理由が `値幅制限内の単価を入力してください` としか出ず、
// **どちらの脚が外なのか・帯がいくつなのかが分からないまま 2 回叩いて 2 回落ちた**。
func TestArmProtectiveOrder_RefusesWhenStopLossOutsideTodaysPriceLimit(t *testing.T) {
	repo := armedRepo(t)
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{}},
		requireExec:      order.ExecMarginSystem,
	}
	// 前日終値 2,803 → 帯は 2,303〜3,303。SL 2,000 は下に外れている。
	arm := newArm(repo, brk, armNow).
		WithPriceLimitRef(func(context.Context, string) float64 { return 2803 })

	_, err := arm.Execute(context.Background(),
		ArmProtectiveInput{Symbol: "4704", TakeProfit: 0, StopLoss: 2000})
	if err == nil {
		t.Fatal("SL が帯の外なのに error にならなかった — 置きに行っても注文ごと拒否される")
	}
	for _, want := range []string{"2303", "3303", "2000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error に %q が無い(帯と入力値を人間に返すこと): %v", want, err)
		}
	}
	if len(brk.placed) != 0 {
		t.Errorf("発注 %d 件 — 帯の外と分かっているので broker を叩かない", len(brk.placed))
	}
}

// 🛑 **基準値段が分からないときは何も変えない。**「判定できない」を理由に守りの形を
// 勝手に変えると、日足が欠けた日だけ TP が消える(ExecuteOrder と同じ縮退)。
func TestArmProtectiveOrder_MultidayIsStopOnlyWhenRefPriceUnknown(t *testing.T) {
	repo := armedRepo(t)
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{}},
		requireExec:      order.ExecMarginSystem,
	}
	arm := newArm(repo, brk, armNow).
		WithPriceLimitRef(func(context.Context, string) float64 { return 0 })

	if _, err := arm.Execute(context.Background(),
		ArmProtectiveInput{Symbol: "4704", TakeProfit: 9999, StopLoss: 1}); err != nil {
		t.Fatalf("判定材料が無いだけで落とさない: %v", err)
	}
	// 基準値段が無くても多日は stop-only(方針は判定材料の有無に依らない)。
	// SL は人間の指定のまま。
	if brk.placed[0].TakeProfit != 0 || brk.placed[0].StopLoss != 1 {
		t.Errorf("ref=0: tp=%v sl=%v, want 0 / 1(多日は stop-only・SL は変えない)",
			brk.placed[0].TakeProfit, brk.placed[0].StopLoss)
	}
}
