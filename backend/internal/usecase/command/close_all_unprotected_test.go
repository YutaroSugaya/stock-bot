package command

import (
	"context"
	"fmt"
	"testing"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// nakedSettleBroker は実際の立花を模す: 決済の約定が非同期で、
// ClosePosition は **受理**を返すが、注文は約定も板への常駐もしない。
//
// 🛑 GetActiveOrders を Paper へ委譲しない。paper の CancelOrder は板から注文を
// 消さない no-op なので、委譲すると「守りを cancel した後の空の板」を再現できない。
type nakedSettleBroker struct {
	*broker.Paper
	resting []order.Order // 板に載っている注文。nil = 空 = 逆指値も決済注文も無い
}

func (b *nakedSettleBroker) SettleFillsAsync() bool { return true }

func (b *nakedSettleBroker) ClosePosition(context.Context, port.CloseRequest) (*port.CloseResult, error) {
	return &port.CloseResult{OrderID: "settle-1", Accepted: true}, nil // 受理のみ(価格は非同期)
}

func (b *nakedSettleBroker) ResolveExecution(_ context.Context, id string) (port.ResolvedExecution, error) {
	if id == "settle-1" {
		return port.ResolvedExecution{}, fmt.Errorf("settle: %w", port.ErrOrderNotFilled)
	}
	return b.Paper.ResolveExecution(context.Background(), id)
}

func (b *nakedSettleBroker) GetActiveOrders(context.Context, string) ([]order.Order, error) {
	return b.resting, nil
}

// 🚨 live で踏んだ事故。画面の「成行決済」は
// POST /api/live/positions/close → LiveViews.Close.CloseOne → CloseAllOpen へ落ちる。
// closeOne は守りの脚を cancel してから決済を出すので、決済が「受理されたのに板に
// 載らない」と、その建玉は逆指値も決済注文も持たない = 完全に裸になる。
//
// 🛑 このテストは **NewCloseAllOpen(本番のコンストラクタ)経由**で組む。
// 763782c は closeExecutor を手で組むテストしか持たなかったため、コンストラクタが
// emergency: nil をリテラルで固定している事実を 1 つも見ておらず、
// **本番配線では一度も発火しない trip** を緑のまま通していた。
func TestCloseAllOpen_UnprotectedSettleTripsTheAlarm(t *testing.T) {
	ctx := context.Background()
	now := closeAllNow()
	h := newCloseHarness(t, now, func(p *broker.Paper) port.Broker {
		return &nakedSettleBroker{Paper: p, resting: nil} // 板は空
	})
	id := h.open(t, "7203", 2500, position.SourceBot, order.HoldingMultiday, order.ExecMarginSystem)

	if _, err := h.cmd.CloseOne(ctx, id); err == nil {
		t.Fatal("precondition: 約定を確認できない決済は成功として返してはいけない")
	}
	if !h.emergency.Active() {
		t.Fatal("守りを cancel した後に決済が板に無い = 建玉が裸。警報を鳴らして人間を呼ばねばならない")
	}
	if got := h.emergency.Reason(); got == "" {
		t.Fatal("trip の理由が空 — 何が起きたか人間に伝わらない")
	}
}

// 逆に、決済注文が板に残っているなら鳴らさない。**未約定の決済を板に残すのは
// 意図的な設計**(ストップ安の引けは比例配分で、板に出ている売り注文にしか配分
// されない)。ここで鳴らすと、正常な待ちのたびに実弾の新規建てが止まる。
//
// 🛑 成行の返済注文は **逆指値脚を持たない**。述語を HasStopLeg で絞ると
// 必ずここを取りこぼす。
func TestCloseAllOpen_RestingSettleDoesNotTripTheAlarm(t *testing.T) {
	ctx := context.Background()
	now := closeAllNow()
	h := newCloseHarness(t, now, func(p *broker.Paper) port.Broker {
		return &nakedSettleBroker{Paper: p, resting: []order.Order{{
			OrderID: "settle-1", Symbol: "7203", Side: order.SideSell,
			Type: order.OrderTypeMarket, Quantity: 100, Status: "working",
		}}}
	})
	id := h.open(t, "7203", 2500, position.SourceBot, order.HoldingMultiday, order.ExecMarginSystem)

	if _, err := h.cmd.CloseOne(ctx, id); err == nil {
		t.Fatal("precondition: 約定を確認できない決済は成功として返してはいけない")
	}
	if h.emergency.Active() {
		t.Fatalf("決済注文が板に残っているのは正常な待ち。鳴らしてはいけない: %q", h.emergency.Reason())
	}
}

// 🛑 警報を配線しない構成(research / harvest = 紙)では鳴らない。
// 紙の close 失敗で測定用の bot を止めない、という既存の事前コミットを守る。
//
// あわせて **警報が nil のときは裸判定の板照会を投げない**ことを見る。
// 絶対回数ではなく **警報あり / なしの差**で見るのは、cancelProtectiveLegs が
// 脚を探すぶんの照会が別に 1 回あり、そちらは警報と無関係に必要だから
// (絶対値で固定すると、脚の探し方を変えたときに無関係な理由で落ちる)。
// 立花の API 予算は 10,000 回/日 で現在 約7,700 回使用中。
func TestCloseAllOpen_NoAlarmWiredStaysSilentAndSkipsTheOrderQuery(t *testing.T) {
	ctx := context.Background()
	now := closeAllNow()

	run := func(alarm bool) (tripped bool, queries int) {
		mk := func(p *broker.Paper) port.Broker {
			return &countingNakedBroker{
				nakedSettleBroker: nakedSettleBroker{Paper: p, resting: nil},
				queried:           &queries,
			}
		}
		h := newCloseHarness(t, now, mk)
		if !alarm {
			h = newCloseHarnessWithoutAlarm(t, now, mk)
		}
		id := h.open(t, "7203", 2500, position.SourceBot, order.HoldingMultiday, order.ExecMarginSystem)
		if _, err := h.cmd.CloseOne(ctx, id); err == nil {
			t.Fatal("precondition: 約定を確認できない決済は成功として返してはいけない")
		}
		return h.emergency.Active(), queries
	}

	silentTripped, silentQueries := run(false)
	armedTripped, armedQueries := run(true)

	if silentTripped {
		t.Fatal("警報を配線していない構成で鳴ってはいけない(紙の close 失敗で bot を止めない)")
	}
	if !armedTripped {
		t.Fatal("警報を配線した構成では鳴らねばならない(この対比が成り立って初めて上の主張に意味がある)")
	}
	if silentQueries >= armedQueries {
		t.Fatalf("鳴らせない構成でも裸判定の板照会を投げている(なし=%d / あり=%d)— "+
			"nil 判定を述語より先に置くこと", silentQueries, armedQueries)
	}
}

type countingNakedBroker struct {
	nakedSettleBroker
	queried *int
}

func (b *countingNakedBroker) GetActiveOrders(ctx context.Context, sym string) ([]order.Order, error) {
	*b.queried++
	return b.nakedSettleBroker.GetActiveOrders(ctx, sym)
}
