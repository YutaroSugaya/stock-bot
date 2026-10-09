package command

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/safety"
)

// errActiveOrders は照会そのものが失敗する境界スタブ。
// MOCK rationale (TESTING.md §1 system boundary): 注文一覧は wire で、障害を
// 再現する手段が他に無い。
type errActiveOrders struct{}

func (errActiveOrders) GetActiveOrders(context.Context, string) ([]order.Order, error) {
	return nil, fmt.Errorf("broker down")
}

// 🛑 「置いたと覚えている」は「板に乗っている」ではない。
//
// `Tachibana.ResolveSettleLegs` は **プロセス内 map** を読むだけで broker に問い合わせない
// (`t.settleLegs[bpID]`)。つまり `PlaceSettleOCO` が成功を返しさえすれば、注文が実際には
// 板に無くても entry saga は素通りし、**無防備な実弾建玉が残る**。これがデモ裏取りで
// 潰すはずだった沈黙の失敗そのもの。
//
// 建玉照会(`GetActiveOrders`)は実 wire なので、そちらで実在を確かめる。
func TestProtectiveOrderIsResting(t *testing.T) {
	cases := []struct {
		name   string
		orders []order.Order
		want   bool
	}{
		{"決済側の逆指値が残っている", []order.Order{
			{OrderID: "o1", Symbol: "7203", Side: order.SideSell, Quantity: 100, HasStopLeg: true},
		}, true},
		{"数量が多い(部分でも守りはある)", []order.Order{
			{OrderID: "o1", Symbol: "7203", Side: order.SideSell, Quantity: 200, HasStopLeg: true},
		}, true},
		{"板に何も無い = 沈黙の失敗", nil, false},
		{"建玉と同じ側しかない(守りではない)", []order.Order{
			{OrderID: "o1", Symbol: "7203", Side: order.SideBuy, Quantity: 100, HasStopLeg: true},
		}, false},
		{"別銘柄の注文は数えない", []order.Order{
			{OrderID: "o1", Symbol: "6758", Side: order.SideSell, Quantity: 100, HasStopLeg: true},
		}, false},
		{"数量が足りない = 一部しか守られていない", []order.Order{
			{OrderID: "o1", Symbol: "7203", Side: order.SideSell, Quantity: 99, HasStopLeg: true},
		}, false},
		// 🛑 ここが「決済注文がある」と「守られている」の分かれ目。利確指値だけが
		// 板にある建玉は下方向に裸で、SL は存在しない。数量で数えると通ってしまう。
		{"利確指値だけ(逆指値脚なし)は守りではない", []order.Order{
			{OrderID: "tp", Symbol: "7203", Side: order.SideSell, Quantity: 100},
		}, false},
		{"利確指値 + 逆指値 なら守りは在る", []order.Order{
			{OrderID: "tp", Symbol: "7203", Side: order.SideSell, Quantity: 100},
			{OrderID: "sl", Symbol: "7203", Side: order.SideSell, Quantity: 100, HasStopLeg: true},
		}, true},
		{"逆指値の数量が足りない(利確指値で嵩上げしない)", []order.Order{
			{OrderID: "tp", Symbol: "7203", Side: order.SideSell, Quantity: 100},
			{OrderID: "sl", Symbol: "7203", Side: order.SideSell, Quantity: 50, HasStopLeg: true},
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := protectiveOrderIsResting(c.orders, "7203", order.SideSell, 100)
			if got != c.want {
				t.Errorf("protectiveOrderIsResting = %v, want %v", got, c.want)
			}
		})
	}
}

// 🛑 照会そのものが失敗したときは **守りが無い側に倒す**(= 拒否)。
// 「確かめられなかった」を「大丈夫だった」と読むと、この検査は障害時に自動で無効化される。
func TestVerifyProtectiveOrder_FailsClosedOnQueryError(t *testing.T) {
	if verifyProtectiveOrder(context.Background(), errActiveOrders{}, "7203", order.SideSell, 100) == nil {
		t.Fatal("照会エラーを通してはいけない(fail-close)")
	}
}

// 🛑 **research(紙)を壊していないこと**。守りの実在確認は entry saga の必須段に
// なったので、paper broker が OCO レッグを注文一覧に出さなければ**全エントリーが
// 補償クローズされる** = 研究モードの forward 収集が静かに止まる。
// paper で建玉が実際に開くことをここで固定する。
func TestEntrySagaStillOpensOnPaper(t *testing.T) {
	ctx := context.Background()
	c := clock.Fixed(time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST))
	pb := broker.NewPaper(c, 0, 0)
	pb.SetPrice("7203", 2500)
	repo := repository.NewInMemoryPositionRepo()
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "f.flag"), nil)

	exec := NewExecuteOrder(pb, repo, safety.NewPendingPositions(), es, c)
	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "7203",
		EntryPrice: 2500, TakeProfitJPY: 100, StopLossJPY: 50, Quantity: 100,
		HoldingMode: order.HoldingIntraday,
	}
	id, err := exec.Execute(ctx, ExecuteOrderInput{
		Signal: sig, Quantity: 100, ExecKind: order.ExecCash, Source: position.SourceBot,
	})
	if err != nil {
		t.Fatalf("paper のエントリーが守り確認で落ちた(research が止まる): %v", err)
	}
	open, _ := repo.ListOpenOrClosing(ctx, "7203")
	if id == 0 || len(open) != 1 {
		t.Fatalf("建玉が開いていない: id=%d open=%d", id, len(open))
	}
}
