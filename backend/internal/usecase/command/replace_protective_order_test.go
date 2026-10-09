package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
)

// MOCK rationale (TESTING.md §1 system boundary): broker / emergency は外部境界。
type fakeReplaceBroker struct {
	fakeExpiryBroker
	placed   []port.OCOCloseOrderInput
	placeErr error
	// requireExec は「お預かり銘柄の区分」。立花は返済注文の口座区分が建玉と
	// 一致しないと**注文ごと拒否**する。zero = 検査しない(旧テスト互換)。
	requireExec order.ExecKind
}

// 🚨 **立花の実挙動を写す**。区分違いの返済は
// 「選択した口座区分がお預かり銘柄と不一致のため、このご注文はお受けできません。」
// で拒否される。fake が入力を検査せず素通しさせると、**現物区分で守りを
// 出し直すバグがテスト緑のまま本番に出て、live の建玉を裸にする**。
// 境界を mock するときは、その境界が拒否する条件も一緒に写すこと。
func (f *fakeReplaceBroker) PlaceSettleOCO(_ context.Context, in port.OCOCloseOrderInput) (string, error) {
	if f.placeErr != nil {
		return "", f.placeErr
	}
	if f.requireExec != "" && in.ExecKind != f.requireExec {
		return "", errors.New("選択した口座区分がお預かり銘柄と不一致のため、このご注文はお受けできません。")
	}
	f.placed = append(f.placed, in)
	return "new-order-1", nil
}

type fakeTripper struct{ reasons []string }

func (f *fakeTripper) Trip(reason string, _ time.Time) error {
	f.reasons = append(f.reasons, reason)
	return nil
}
func (f *fakeTripper) Active() bool { return len(f.reasons) > 0 }

// 🛑 **取引時間帯を入れた TradingHours**。renew 側の testHours() は TZ と
// CalendarThrough しか持たず、InTradingHours が常に false になる。それだと
// 「場中は撃たない」というこのコマンドの中心的な約束をテストが素通りさせる
// (実際に素通りした)。
func replaceHours() session.TradingHours {
	h := testHours()
	h.Sessions = []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:30"}}
	return h
}

func guardAtCeiling() port.ProtectiveOrderInfo {
	return port.ProtectiveOrderInfo{
		OrderID: "13014332", Symbol: "4901", HasStopLeg: true,
		Side: order.SideSell, Quantity: 100,
		ExpireOn:  time.Date(2026, 8, 26, 0, 0, 0, 0, clock.JST),
		BrokerRef: "20260813",
		// 🛑 人間が手で締めた値段。**これを引き継ぐ**。
		LimitPrice: 3329, StopTrigger: 2986.5,
	}
}

// 🚨 **訂正では発注日+10営業日の天井を超えられない**。
// 期日を延ばす唯一の手段が「取消 → 同条件で再発注」になった。
//
// 🛑 **いま板にある値段を引き継ぐ**。Position の凍結値を使うと、人間が手で締めた
// TP/SL を勝手に元の広い幅へ戻してしまう(人間が
// 手で締めた値を尊重する)。
func TestReplaceProtectiveOrder_CancelsThenRePlacesWithTheSamePrices(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4901", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	brk := &fakeReplaceBroker{fakeExpiryBroker: fakeExpiryBroker{
		orders: map[string][]port.ProtectiveOrderInfo{"4901": {guardAtCeiling()}},
	}}
	trip := &fakeTripper{}
	// 🛑 場外(寄り前)。値段が動かない時間帯にしか撃たない。
	now := time.Date(2026, 8, 25, 8, 10, 0, 0, clock.JST)

	r := NewReplaceProtectiveOrder(repo, brk, replaceHours(), func() time.Time { return now }, trip)
	res, errs := r.Execute(context.Background())

	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if res.Replaced != 1 {
		t.Fatalf("replaced = %d, want 1", res.Replaced)
	}
	if len(brk.cancelled) != 1 || brk.cancelled[0] != "13014332" {
		t.Errorf("取消が先に走っていない: %v", brk.cancelled)
	}
	if len(brk.placed) != 1 {
		t.Fatalf("再発注が %d 件", len(brk.placed))
	}
	got := brk.placed[0]
	// 多日建玉の板の守りは **stop-only**。板の TP 3329 は引き継がず、SL だけ
	// 同条件で置き直す(翌朝の繰越で TP が帯の外に出ると SL ごと失効するため)。
	if got.TakeProfit != 0 || got.StopLoss != 2986.5 {
		t.Errorf("値段 TP=%v SL=%v, want 0 / 2986.5 — 多日は stop-only、SL は人間が手で締めた幅のまま", got.TakeProfit, got.StopLoss)
	}
	// 新しい期日 = 発注日(=今日)から 9 営業日先。8/25 → 9/7。
	want := time.Date(2026, 9, 7, 0, 0, 0, 0, clock.JST)
	if !got.ExpireOn.Equal(want) {
		t.Errorf("新しい期日 = %s, want %s", got.ExpireOn.Format("2006-01-02"), want.Format("2006-01-02"))
	}
	if trip.Active() {
		t.Errorf("成功したのに emergency を trip した: %v", trip.reasons)
	}
}

// 🛑 **場中は絶対に撃たない。** 取消と再発注の間は守りが完全に消える窓で、
// 場が開いていればその窓で値が飛びうる。場が閉じている時間帯にしか許さない。
func TestReplaceProtectiveOrder_RefusesDuringTradingHours(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4901", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	brk := &fakeReplaceBroker{fakeExpiryBroker: fakeExpiryBroker{
		orders: map[string][]port.ProtectiveOrderInfo{"4901": {guardAtCeiling()}},
	}}
	now := time.Date(2026, 8, 25, 10, 30, 0, 0, clock.JST) // 場中

	r := NewReplaceProtectiveOrder(repo, brk, replaceHours(), func() time.Time { return now }, &fakeTripper{})
	res, _ := r.Execute(context.Background())

	if res.Replaced != 0 || len(brk.cancelled) != 0 || len(brk.placed) != 0 {
		t.Errorf("場中に撃った: replaced=%d cancelled=%v placed=%d", res.Replaced, brk.cancelled, len(brk.placed))
	}
}

// 🚨 **取消は通ったのに再発注が失敗 = 建玉が裸のまま残る。**
// ここは黙って次へ進んではいけない。emergency を trip して新規を止め、叫ぶ。
func TestReplaceProtectiveOrder_TripsEmergencyWhenRePlaceFails(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4901", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{"4901": {guardAtCeiling()}}},
		placeErr:         errors.New("証拠金不足"),
	}
	trip := &fakeTripper{}
	now := time.Date(2026, 8, 25, 8, 10, 0, 0, clock.JST)

	r := NewReplaceProtectiveOrder(repo, brk, replaceHours(), func() time.Time { return now }, trip)
	res, errs := r.Execute(context.Background())

	if res.Replaced != 0 {
		t.Errorf("replaced = %d, want 0", res.Replaced)
	}
	if len(errs) == 0 {
		t.Fatal("再発注が失敗したのに黙った — 建玉が裸のまま残っている")
	}
	if !trip.Active() {
		t.Error("emergency を trip していない — 守りが無い状態で新規が建ち続ける")
	}
	joined := strings.Join(trip.reasons, ",")
	if !strings.Contains(joined, "4901") {
		t.Errorf("trip の理由に銘柄が無い: %v", trip.reasons)
	}
}

// 取消そのものが失敗したら**再発注しない**(守りは板に残っている = 現状維持が正しい)。
func TestReplaceProtectiveOrder_DoesNotPlaceWhenCancelFails(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4901", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	brk := &fakeReplaceBroker{fakeExpiryBroker: fakeExpiryBroker{
		orders:    map[string][]port.ProtectiveOrderInfo{"4901": {guardAtCeiling()}},
		cancelErr: map[string]error{"13014332": errors.New("取消拒否")},
	}}
	trip := &fakeTripper{}
	now := time.Date(2026, 8, 25, 8, 10, 0, 0, clock.JST)

	r := NewReplaceProtectiveOrder(repo, brk, replaceHours(), func() time.Time { return now }, trip)
	_, errs := r.Execute(context.Background())

	if len(brk.placed) != 0 {
		t.Error("取消が失敗したのに再発注した — 守りが二重になる")
	}
	if len(errs) == 0 {
		t.Error("取消失敗を黙った")
	}
	if trip.Active() {
		t.Error("取消失敗で trip した — 守りは板に残っているので新規を止める必要は無い")
	}
}

// 🚨 **区分と建玉の紐付けを引き継ぐ**(本番事故の回帰テスト)。
//
// 再発注の入力を板の注文情報だけから組んでいたため `ExecKind` がゼロ値 "" になり、
// adapter の `genkinShinyouKubun` が default 節に落ちて **"0"(現物)** を送っていた。
// 建玉は制度信用なので立花は注文ごと拒否 —— **取消は通った後**だったので、4704 /
// 4751 / 4901 の 3 建玉が寄り前に守り無しで取り残された。
//
// 🛑 **値段は板から、区分と建玉 ID は台帳から。**出どころが 2 つあることを
// テストで固定する(片方だけ見ていたのが事故の原因そのもの)。
func TestReplaceProtectiveOrder_CarriesTheFrozenExecKindAndPositionID(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4901", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{
			orders: map[string][]port.ProtectiveOrderInfo{"4901": {guardAtCeiling()}},
		},
		requireExec: order.ExecMarginSystem, // お預かりは制度信用
	}
	now := time.Date(2026, 8, 25, 8, 10, 0, 0, clock.JST)

	r := NewReplaceProtectiveOrder(repo, brk, replaceHours(), func() time.Time { return now }, &fakeTripper{})
	res, errs := r.Execute(context.Background())

	if len(errs) != 0 {
		t.Fatalf("errs = %v — 区分違いで拒否されている", errs)
	}
	if res.Replaced != 1 {
		t.Fatalf("replaced = %d, want 1", res.Replaced)
	}
	if got := brk.placed[0].ExecKind; got != order.ExecMarginSystem {
		t.Fatalf("ExecKind = %q, want %q — 建玉の凍結区分を引き継いでいない(現物で返済を撃つ)",
			got, order.ExecMarginSystem)
	}
	if got := brk.placed[0].BrokerPositionID; got != "b-4901" {
		t.Fatalf("BrokerPositionID = %q, want \"b-4901\" — 再発注が建玉に紐付いていない", got)
	}
}

// 🛑 **区分が分からないなら取り消さない。** 台帳から建玉を特定できないまま
// 取消を撃つと、再発注が組めずに裸で残る —— 実際に起きた形。
// 「取消してから考える」は守りの経路では常に間違い。
func TestReplaceProtectiveOrder_DoesNotCancelWhenThePositionCannotBeResolved(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4901", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	// 数量が建玉と合わない = どの建玉の守りか確定できない。
	odd := guardAtCeiling()
	odd.Quantity = 300
	brk := &fakeReplaceBroker{fakeExpiryBroker: fakeExpiryBroker{
		orders: map[string][]port.ProtectiveOrderInfo{"4901": {odd}},
	}}
	now := time.Date(2026, 8, 25, 8, 10, 0, 0, clock.JST)

	r := NewReplaceProtectiveOrder(repo, brk, replaceHours(), func() time.Time { return now }, &fakeTripper{})
	res, errs := r.Execute(context.Background())

	if res.Replaced != 0 {
		t.Fatalf("replaced = %d, want 0", res.Replaced)
	}
	if len(brk.cancelled) != 0 {
		t.Fatalf("cancelled = %v — 建玉を特定できないのに取消を撃った(裸になる)", brk.cancelled)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "取り消さない") {
		t.Fatalf("errs = %v — 特定できないことが error として出ていない", errs)
	}
}

// 🛑 **期日が近い守りだけ触る**(残り 2 営業日)。
//
// 置き直しは取消と再発注の間に**守りが完全に消える窓**を開ける。毎朝すべての守りを
// 無条件に置き直すと、その窓を毎日・全建玉ぶん開けることになる。窓を開ける回数は
// 「期日が切れる直前の 1 回」で足りる。
func TestReplaceProtectiveOrder_OnlyTouchesGuardsNearExpiry(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4704", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	// 8/25(火)の 2 営業日先 = 8/27。8/31 はまだ先なので触らない。
	far := guardAtCeiling()
	far.OrderID, far.Symbol = "far", "4704"
	far.ExpireOn = time.Date(2026, 8, 31, 0, 0, 0, 0, clock.JST)
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{"4704": {far}}},
		requireExec:      order.ExecMarginSystem,
	}
	now := time.Date(2026, 8, 25, 8, 10, 0, 0, clock.JST)

	r := NewReplaceProtectiveOrder(repo, brk, replaceHours(), func() time.Time { return now }, &fakeTripper{})
	res, errs := r.Execute(context.Background())

	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if res.Replaced != 0 {
		t.Fatalf("Replaced = %d — まだ余裕がある守りを置き直した(無用な裸の窓を開けた)", res.Replaced)
	}
	if res.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1 — 見送ったことが呼び手に伝わらない", res.Skipped)
	}
	if len(brk.cancelled) != 0 {
		t.Fatalf("cancelled = %v — 余裕がある守りを取り消した", brk.cancelled)
	}
}

// 期日が閾値の内側に入ったら置き直す。8/26 は 8/25 の 2 営業日先(8/27)以内。
func TestReplaceProtectiveOrder_ReplacesOnceTheDeadlineIsInside(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4901", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{"4901": {guardAtCeiling()}}},
		requireExec:      order.ExecMarginSystem,
	}
	now := time.Date(2026, 8, 25, 8, 10, 0, 0, clock.JST)

	r := NewReplaceProtectiveOrder(repo, brk, replaceHours(), func() time.Time { return now }, &fakeTripper{})
	res, errs := r.Execute(context.Background())

	if len(errs) != 0 || res.Replaced != 1 {
		t.Fatalf("Replaced=%d errs=%v — 期日が近いのに置き直していない", res.Replaced, errs)
	}
	if res.Skipped != 0 {
		t.Fatalf("Skipped = %d, want 0", res.Skipped)
	}
}

// 🛑 当日限り(zero)は「無期限」ではなく**最も危険**。必ず対象にする。
func TestReplaceProtectiveOrder_TreatsSameDayExpiryAsDue(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4901", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	today := guardAtCeiling()
	today.ExpireOn = time.Time{} // 当日限り / 期日が読めない
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{"4901": {today}}},
		requireExec:      order.ExecMarginSystem,
	}
	now := time.Date(2026, 8, 25, 8, 10, 0, 0, clock.JST)

	r := NewReplaceProtectiveOrder(repo, brk, replaceHours(), func() time.Time { return now }, &fakeTripper{})
	res, _ := r.Execute(context.Background())
	if res.Replaced != 1 {
		t.Fatalf("Replaced = %d — 当日限りの守りを『余裕がある』と読んだ", res.Replaced)
	}
}

// 🚨 前日の板の TP を**今日の帯**で検問してから取り消す。
// 前日終値が動いて帯がずれると、板から引き継いだ TP が外に出る(7220)。
// 取消の後で拒否されると裸 + trip なので、TP 脚は取消の前に落とす。
func TestReplaceProtectiveOrder_DropsTakeProfitOutsideTodaysBand(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4901", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	brk := &fakeReplaceBroker{fakeExpiryBroker: fakeExpiryBroker{
		orders: map[string][]port.ProtectiveOrderInfo{"4901": {guardAtCeiling()}}, // TP 3329 / SL 2986.5
	}}
	trip := &fakeTripper{}
	now := time.Date(2026, 8, 25, 8, 10, 0, 0, clock.JST)
	const ref = 2803.0 // 帯 2,303〜3,303 → TP 3329 は外、SL 2986.5 は内
	if up, _, _ := risk.LimitBandFor(ref); up >= 3329 {
		t.Fatalf("precondition: 帯の上限 %v が TP 3329 を含んでしまう", up)
	}

	r := NewReplaceProtectiveOrder(repo, brk, replaceHours(), func() time.Time { return now }, trip).
		WithPriceLimitRef(func(context.Context, string) float64 { return ref })
	res, errs := r.Execute(context.Background())
	if len(errs) != 0 || res.Replaced != 1 || len(brk.placed) != 1 {
		t.Fatalf("errs=%v replaced=%d placed=%d", errs, res.Replaced, len(brk.placed))
	}
	if got := brk.placed[0]; got.TakeProfit != 0 || got.StopLoss != 2986.5 {
		t.Fatalf("TP=%v SL=%v — 帯の外の TP 脚だけ落として SL は引き継ぐはず", got.TakeProfit, got.StopLoss)
	}
	if trip.Active() {
		t.Fatalf("trip した: %v", trip.reasons)
	}
}

// 🚨 SL が今日の帯の外なら**取り消さない**。板に残っている守りは(期日まで)まだ守りで、
// 取り消した瞬間に確実に裸になる。人間に帯の数字を返す。
func TestReplaceProtectiveOrder_DoesNotCancelWhenStopIsOutsideTodaysBand(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	seedMultiday(t, repo, "4901", time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST))
	brk := &fakeReplaceBroker{fakeExpiryBroker: fakeExpiryBroker{
		orders: map[string][]port.ProtectiveOrderInfo{"4901": {guardAtCeiling()}},
	}}
	trip := &fakeTripper{}
	now := time.Date(2026, 8, 25, 8, 10, 0, 0, clock.JST)
	const ref = 4000.0 // 帯 3,300〜4,700 → SL 2986.5 は外
	if _, down, _ := risk.LimitBandFor(ref); down <= 2986.5 {
		t.Fatalf("precondition: 帯の下限 %v が SL 2986.5 を含んでしまう", down)
	}

	r := NewReplaceProtectiveOrder(repo, brk, replaceHours(), func() time.Time { return now }, trip).
		WithPriceLimitRef(func(context.Context, string) float64 { return ref })
	_, errs := r.Execute(context.Background())
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want 1(SL が帯の外)", errs)
	}
	if len(brk.cancelled) != 0 || len(brk.placed) != 0 {
		t.Fatalf("帯の外の SL で取消/再発注に進んだ: cancelled=%v placed=%d", brk.cancelled, len(brk.placed))
	}
	if trip.Active() {
		t.Fatalf("板の守りは無傷なのに trip した: %v", trip.reasons)
	}
}
