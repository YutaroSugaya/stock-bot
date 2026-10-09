package command

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 🚨 **板の SL を trail の利確の線まで引き上げる**。
// trail の利確の線(+1×ATR 以上)は bot の OnTick だけが持ち、板の SL は −2×ATR のまま。
// bot が場中に居ないと、線を割っても誰も売らず、板の SL まで落ちる(差は 3×ATR)。
// 寄り前に、線が立っている建玉だけ板の SL を線の値段へ上げる。上げるだけで下げない。
// 取消 → 再発注・帯の検問・台帳の更新は RepriceProtectiveOrder が持つ(作り直さない)。

var raiseNow = time.Date(2026, 10, 5, 8, 0, 0, 0, clock.JST) // 月曜の寄り前

type raiseFixture struct {
	repo *repository.InMemoryPositionRepo
	brk  *fakeReplaceBroker
	trip *fakeTripper
	id   int64
}

// 建値 2000・arm 100(= 1×ATR)・giveback 150・SL 1800(2×ATR)の trail 建玉と、板の逆指値 1800。
func newRaiseFixture(t *testing.T, peak float64, armed bool, boardSL float64) raiseFixture {
	t.Helper()
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	id, err := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "b-5726", Symbol: "5726", Side: order.SideBuy, Quantity: 100, EntryPrice: 2000,
		OpenedAt: time.Date(2026, 9, 30, 9, 0, 0, 0, clock.JST), HoldingMode: order.HoldingMultiday,
		ExecKind: order.ExecMarginSystem, StrategyName: "bnf_reversion_trail",
		StopLossJPY: 200, StopLossPrice: 1800,
		RatchetArmJPY: 100, RatchetGivebackJPY: 150, RatchetFloorAtArm: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateExcursion(ctx, id, peak, -20, armed); err != nil {
		t.Fatal(err)
	}
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{"5726": {{
			OrderID: "o-5726", Symbol: "5726", HasStopLeg: true, Side: order.SideSell, Quantity: 100,
			ExpireOn: time.Date(2026, 10, 9, 0, 0, 0, 0, clock.JST), BrokerRef: "20260930", StopTrigger: boardSL,
		}}}},
		requireExec: order.ExecMarginSystem,
	}
	return raiseFixture{repo: repo, brk: brk, trip: &fakeTripper{}, id: id}
}

func (f raiseFixture) run(t *testing.T, now time.Time, prevClose float64) (RaiseTrailStopsResult, []error) {
	t.Helper()
	clk := func() time.Time { return now }
	ref := func(context.Context, string) float64 { return prevClose }
	reprice := NewRepriceProtectiveOrder(f.repo, f.brk, replaceHours(), clk, f.trip).WithPriceLimitRef(ref)
	return NewRaiseTrailStops(f.repo, f.brk, replaceHours(), clk, reprice).WithPriceLimitRef(ref).Execute(context.Background())
}

func TestRaiseTrailStops_RaisesTheBoardStopToTheLineBeforeTheOpen(t *testing.T) {
	f := newRaiseFixture(t, 180, true, 1800)
	res, errs := f.run(t, raiseNow, 2150)
	if len(errs) != 0 || res.Raised != 1 {
		t.Fatalf("raised=%d errs=%v, want 1 本・エラー無し", res.Raised, errs)
	}
	if len(f.brk.cancelled) != 1 || f.brk.cancelled[0] != "o-5726" {
		t.Fatalf("cancelled=%v — 板の守りを取り消していない", f.brk.cancelled)
	}
	got := f.brk.placed[0]
	if got.StopLoss != 2100 || got.TakeProfit != 0 || got.ExecKind != order.ExecMarginSystem {
		t.Fatalf("置いた守り = SL %g / TP %g / %s, want SL 2100(線)/ TP 0(多日は stop-only)/ 制度信用", got.StopLoss, got.TakeProfit, got.ExecKind)
	}
	p, err := f.repo.GetByID(context.Background(), f.id)
	if err != nil || p.StopLossPrice != 2100 {
		t.Fatalf("台帳の SL = %v(err=%v), want 2100 — 板と台帳が食い違うと、守りが消えたとき古い SL で置き直される", p.StopLossPrice, err)
	}
	if len(f.trip.reasons) != 0 {
		t.Fatalf("trip した: %v", f.trip.reasons)
	}
}

func TestRaiseTrailStops_LeavesTheBoardAlone(t *testing.T) {
	cases := []struct {
		name      string
		peak      float64
		armed     bool
		boardSL   float64
		now       time.Time
		prevClose float64
	}{
		{"線が立っていない(arm 前)", 50, false, 1800, raiseNow, 2150},
		{"板の SL が既に線以上(下げない)", 180, true, 2100, raiseNow, 2150},
		{"場中は撃たない(取消と再発注の間に守りが消える)", 180, true, 1800, time.Date(2026, 10, 5, 10, 30, 0, 0, clock.JST), 2150},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRaiseFixture(t, c.peak, c.armed, c.boardSL)
			res, _ := f.run(t, c.now, c.prevClose)
			if res.Raised != 0 || len(f.brk.cancelled) != 0 || len(f.brk.placed) != 0 {
				t.Fatalf("触ってはいけないのに触った: raised=%d cancelled=%v placed=%v", res.Raised, f.brk.cancelled, f.brk.placed)
			}
		})
	}
}

// 前日終値が既に線以下 = 線を割ったのに bot が売っていない(bot が止まっていた)。
// 売りの逆指値を現値より上に置いたときの立花の挙動は未実測なので**置かない**。名指しで返す。
func TestRaiseTrailStops_DoesNotPlaceAStopAboveThePreviousClose(t *testing.T) {
	f := newRaiseFixture(t, 180, true, 1800)
	res, errs := f.run(t, raiseNow, 2090)
	if res.Raised != 0 || len(f.brk.cancelled) != 0 {
		t.Fatalf("前日終値 2090 < 線 2100 なのに置き直した: raised=%d cancelled=%v", res.Raised, f.brk.cancelled)
	}
	if len(errs) != 1 {
		t.Fatalf("errs=%v — 線を割ったまま残っていることを名指しで返していない", errs)
	}
}

// capped(TP で降りる)は対象外。
func TestRaiseTrailStops_IgnoresCappedPositions(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	id, err := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "b-3186", Symbol: "3186", Side: order.SideBuy, Quantity: 100, EntryPrice: 2871,
		OpenedAt: time.Date(2026, 9, 14, 9, 0, 0, 0, clock.JST), HoldingMode: order.HoldingMultiday,
		ExecKind: order.ExecMarginSystem, StrategyName: "bnf_reversion",
		TakeProfitPrice: 2977, StopLossPrice: 2659,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateExcursion(ctx, id, 113, -129, false); err != nil {
		t.Fatal(err)
	}
	brk := &fakeReplaceBroker{fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{"3186": {{
		OrderID: "o-3186", Symbol: "3186", HasStopLeg: true, Side: order.SideSell, Quantity: 100, StopTrigger: 2659,
	}}}}}
	clk := func() time.Time { return raiseNow }
	reprice := NewRepriceProtectiveOrder(repo, brk, replaceHours(), clk, &fakeTripper{})
	res, _ := NewRaiseTrailStops(repo, brk, replaceHours(), clk, reprice).Execute(ctx)
	if res.Raised != 0 || len(brk.cancelled) != 0 {
		t.Fatalf("capped の守りに触った: raised=%d cancelled=%v", res.Raised, brk.cancelled)
	}
}
