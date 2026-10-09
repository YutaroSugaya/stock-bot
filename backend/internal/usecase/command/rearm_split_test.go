package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

const splitRearmEntry = 2800.0

func splitRearmRepo(t *testing.T) *repository.InMemoryPositionRepo {
	t.Helper()
	repo := repository.NewInMemoryPositionRepo()
	if _, err := repo.Insert(context.Background(), port.PositionInsertInput{
		BrokerPositionID: "b-4704", Symbol: "4704", Side: order.SideBuy, Quantity: 100, EntryPrice: splitRearmEntry,
		OpenedAt:    time.Date(2026, 8, 13, 13, 0, 0, 0, clock.JST),
		HoldingMode: order.HoldingMultiday, MaxHoldMinutes: 14400, ExecKind: order.ExecMarginSystem,
		TakeProfitPrice: seededTakeProfit, StopLossPrice: seededStopLoss, StopLossJPY: splitRearmEntry - seededStopLoss,
	}); err != nil {
		t.Fatal(err)
	}
	return repo
}

func splitBroker(qty int, entry float64) *fakeReplaceBroker {
	return &fakeReplaceBroker{
		fakeExpiryBroker: fakeExpiryBroker{
			orders:      map[string][]port.ProtectiveOrderInfo{},
			heldSymbols: []string{"4704"},
			heldQty:     map[string]int{"4704": qty},
			heldEntry:   map[string]float64{"4704": entry},
		},
		requireExec: order.ExecMarginSystem,
	}
}

// 🚨 権利落ち日の寄り前: 立花は期間指定の守りを失効させ、建玉を 500 株・建単価 1/5 へ言い直す
// (と想定される)。その状態で**分割前の凍結 SL・100 株**で置き直すと、時価の 5 倍の逆指値売り =
// 寄りで即発動して 100 株だけ売れ、残り 400 株が守りの無い建玉になる。
// broker の株数と建単価の**両方**が同じ分割比を示すときだけ、台帳を言い直してから置く。
func TestRearmUnguarded_RestatesLedgerWhenBrokerShowsASplit(t *testing.T) {
	repo := splitRearmRepo(t)
	brk := splitBroker(500, splitRearmEntry/5)
	res, errs := newRearm(repo, brk, armNow).Execute(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if res.Armed != 1 || len(brk.placed) != 1 {
		t.Fatalf("Armed=%d placed=%d, want 1/1", res.Armed, len(brk.placed))
	}
	wantSL := market.RoundToTickOf("4704", seededStopLoss/5)
	if got := brk.placed[0]; got.StopLoss != wantSL || got.Quantity != 500 {
		t.Fatalf("置いた守り = SL %v qty %d, want SL %v qty 500(分割後)", got.StopLoss, got.Quantity, wantSL)
	}
	ps, _ := repo.ListOpenOrClosing(context.Background(), "4704")
	if len(ps) != 1 || ps[0].Quantity != 500 || ps[0].SplitFactor != 5 {
		t.Fatalf("台帳を言い直していない: %+v", ps)
	}
}

// 🛑 株数だけがずれて建単価が変わっていない = 分割ではない(人間の部分返済など)。
// 凍結値で置くと「数量と値段がずれた返済注文」になるので**置かない**・台帳も触らない。
func TestRearmUnguarded_QuantityMismatchWithoutSplitEvidenceDoesNotArm(t *testing.T) {
	for _, c := range []struct {
		name  string
		qty   int
		entry float64
	}{
		{"株数だけ 5 倍(建単価そのまま)", 500, splitRearmEntry},
		{"株数と建単価の比が食い違う", 130, splitRearmEntry / 1.2},
		{"建単価が読めない", 500, 0},
		// 立花は整数倍以外の分割で株数を増やさず建単価だけ下げる。分割前の凍結 SL で置くと寄りで即発動する。
		{"株数そのまま・建単価だけ下がった(整数倍以外の分割)", 100, splitRearmEntry - 400},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo := splitRearmRepo(t)
			brk := splitBroker(c.qty, c.entry)
			res, errs := newRearm(repo, brk, armNow).Execute(context.Background())
			if res.Armed != 0 || len(brk.placed) != 0 {
				t.Fatalf("数量が台帳と一致しないのに置いた: %+v", brk.placed)
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), "株数") {
				t.Fatalf("黙って飛ばした / 理由が違う: %v", errs)
			}
			ps, _ := repo.ListOpenOrClosing(context.Background(), "4704")
			if ps[0].Quantity != 100 {
				t.Fatalf("証拠なしで台帳を書き換えた: qty %d", ps[0].Quantity)
			}
		})
	}
}

// 1:1.2 のような一覧に無い小さい比も、株数と建単価がそろえば分割(独立な 2 つの証拠)。
func TestRearmUnguarded_RestatesSmallSplit(t *testing.T) {
	repo := splitRearmRepo(t)
	brk := splitBroker(120, splitRearmEntry/1.2)
	res, errs := newRearm(repo, brk, armNow).Execute(context.Background())
	if len(errs) != 0 || res.Armed != 1 {
		t.Fatalf("Armed=%d errs=%v", res.Armed, errs)
	}
	if got := brk.placed[0]; got.Quantity != 120 || got.StopLoss != market.RoundToTickOf("4704", seededStopLoss/1.2) {
		t.Fatalf("置いた守り = qty %d SL %v", got.Quantity, got.StopLoss)
	}
}
