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
	"stockbot/backend/internal/port"
)

// 🚨 **live の多日建玉で踏んだ穴。** 多日保有の守りは、置いた翌日以降に **broker 側で勝手に消える**。
// 前日終値が下がって帯がずれ、TP 脚が帯の外に出た結果、立花が
// **繰越で注文を無効化**する(該当注文の状態は「無効」/「繰越失効」)。SL 脚も道連れ。
//
// 🛑 **既存のどの経路も、これを直せなかった。**
//   - `ReplaceProtectiveOrder`(寄り前の置き直し)は**板に注文が在ること**が前提。
//     消えた守りは対象にすらならない
//   - `ArmProtectiveOrder` は**人間が叩く手動経路**
//
// つまり **毎朝の裸検知が「見つける」だけで、直すのは人間の手作業**だった。
// 寄りで消えて人間が翌朝に直すまで —— **丸 1 日ぶん裸**。
func TestRearmUnguarded_ArmsFromTheLedgerWhenTheBoardIsEmpty(t *testing.T) {
	repo := rearmRepo(t)
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{}},
		requireExec:      order.ExecMarginSystem,
	}
	r := newRearm(repo, brk, armNow)

	res, errs := r.Execute(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if res.Armed != 1 {
		t.Fatalf("Armed = %d, want 1 — 板が空なら台帳の凍結値で置き直すはず", res.Armed)
	}
	if len(brk.placed) != 1 {
		t.Fatalf("発注 %d 件 — 1 件のはず", len(brk.placed))
	}
	// 🛑 値段は**台帳の凍結値**から。人間の入力を待たない(待っている間ずっと裸)。
	if got := brk.placed[0].StopLoss; got != seededStopLoss {
		t.Errorf("SL = %v, want %v(凍結値)", got, seededStopLoss)
	}
}

// 🛑 **守りが板にあるなら何もしない。**二重の返済注文は拘束数量の超過で
// **両方弾かれうる** —— 直すつもりが両脚とも消す最悪の形になる。
func TestRearmUnguarded_SkipsWhenTheBoardAlreadyHasAStopLeg(t *testing.T) {
	repo := rearmRepo(t)
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{
			"4704": {{OrderID: "existing", HasStopLeg: true, Side: order.SideSell, Quantity: 100}},
		}},
		requireExec: order.ExecMarginSystem,
	}

	res, errs := newRearm(repo, brk, armNow).Execute(context.Background())
	if len(errs) != 0 {
		t.Fatalf("既に守りがあるのは異常ではない。errs = %v", errs)
	}
	if res.Armed != 0 || res.Skipped != 1 {
		t.Errorf("Armed=%d Skipped=%d, want 0/1", res.Armed, res.Skipped)
	}
	if len(brk.placed) != 0 {
		t.Errorf("発注 %d 件 — 板に守りがあるので 1 本も置かない", len(brk.placed))
	}
}

// 🚨 **置けなかったことを飲み込まない。** ここで黙ると「直したつもりで裸」になり、
// 毎朝の ERROR だけが延々と出続ける。
func TestRearmUnguarded_ReportsFailureLoudlyAndLeavesTheRestAlone(t *testing.T) {
	repo := rearmRepo(t)
	brk := &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{orders: map[string][]port.ProtectiveOrderInfo{}},
		requireExec:      order.ExecMarginSystem,
		placeErr:         errors.New("当該銘柄の値幅制限内の単価を入力してください"),
	}

	res, errs := newRearm(repo, brk, armNow).Execute(context.Background())
	if len(errs) == 0 {
		t.Fatal("発注が拒否されたのに errs が空 — 裸のままなのに黙るのが一番危ない")
	}
	if res.Armed != 0 {
		t.Errorf("Armed = %d, want 0", res.Armed)
	}
	if !strings.Contains(errs[0].Error(), "4704") || !strings.Contains(errs[0].Error(), "裸") {
		t.Errorf("error に銘柄と「裸」が要る(人間が読む唯一の手掛かり): %v", errs[0])
	}
}

const (
	seededTakeProfit = 3345.0
	seededStopLoss   = 2597.0
)

// 🛑 fixture は**凍結値を持つ**。持たない fixture だと「台帳から値段を取る」実装と
// 「0 を送る」実装の区別がつかず、本番で TP/SL が 0 の返済注文になる。
func rearmRepo(t *testing.T) *repository.InMemoryPositionRepo {
	t.Helper()
	repo := repository.NewInMemoryPositionRepo()
	if _, err := repo.Insert(context.Background(), port.PositionInsertInput{
		BrokerPositionID: "b-4704", Symbol: "4704", Side: order.SideBuy, Quantity: 100,
		OpenedAt:    time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST),
		HoldingMode: order.HoldingMultiday, MaxHoldMinutes: 14400,
		ExecKind:        order.ExecMarginSystem,
		TakeProfitPrice: seededTakeProfit, StopLossPrice: seededStopLoss,
	}); err != nil {
		t.Fatal(err)
	}
	return repo
}

func newRearm(repo *repository.InMemoryPositionRepo, brk *fakeReplaceBroker, now time.Time) *RearmUnguarded {
	// 🛑 **broker はこの建玉を持っている。**守りが板から消えたという話であって、
	// 建玉が消えたという話ではない(取り違えると裸の復旧が丸ごと止まる)。
	if brk.heldSymbols == nil {
		brk.heldSymbols = []string{"4704"}
	}
	arm := NewArmProtectiveOrder(repo, brk, armHours(), func() time.Time { return now })
	return NewRearmUnguarded(repo, brk, arm)
}
