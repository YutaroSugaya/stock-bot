package protectiveboard

import (
	"context"
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

type boardStub struct {
	orders map[string][]port.ProtectiveOrderInfo
	err    map[string]error
}

func (b *boardStub) ListProtectiveOrders(_ context.Context, sym string) ([]port.ProtectiveOrderInfo, error) {
	if e := b.err[sym]; e != nil {
		return nil, e
	}
	return b.orders[sym], nil
}

// closingSweepFixture は「決済中の多日建玉が 1 本」の台帳を作る。
func closingSweepFixture(t *testing.T, status position.Status) port.PositionRepository {
	t.Helper()
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	now := boardAt
	id, err := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "shinyo:4704", Symbol: "4704", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 5381, OpenedAt: now,
		Source: position.SourceBot, HoldingMode: order.HoldingMultiday, ExecKind: order.ExecMarginSystem,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if status == position.StatusClosing {
		if ok, err := repo.ClaimForClose(ctx, id, now); err != nil || !ok {
			t.Fatalf("claim: %v", err)
		}
	}
	return repo
}

// 🚨 live で裸になった建玉は 2 度とも **CLOSING で裸**だった。板のスイープは
// CLOSING を明示的に除外していたので、**事故の当事者そのものが唯一の検出器の
// 視界の外**にいた。画面の 🚨 タグにもログの裸警報にも何も出なかった。
func TestProtectiveBoardFlagsNakedClosingPositions(t *testing.T) {
	ctx := context.Background()
	var st State
	repo := closingSweepFixture(t, position.StatusClosing)
	brk := &boardStub{orders: map[string][]port.ProtectiveOrderInfo{"4704": nil}} // 板は空

	Refresh(ctx, repo, brk, &st, boardAt, nil)

	v := st.View()
	cn, _ := v["closing_naked"].([]string)
	if len(cn) != 1 || cn[0] != "4704" {
		t.Fatalf("closing_naked=%v — 決済中で守りも決済注文も無い建玉が検出に出ない", v["closing_naked"])
	}
}

// 決済注文が板に残っているなら裸ではない(決済が進行中)。
// 🛑 逆指値脚の有無で絞らない —— 成行の返済注文は脚を持たないので、絞ると必ず取りこぼす。
func TestProtectiveBoardTreatsARestingSettleAsNotNaked(t *testing.T) {
	ctx := context.Background()
	var st State
	repo := closingSweepFixture(t, position.StatusClosing)
	brk := &boardStub{orders: map[string][]port.ProtectiveOrderInfo{"4704": {{
		OrderID: "settle-1", Symbol: "4704", Side: order.SideSell,
		Quantity: 100, HasStopLeg: false, // 成行の返済注文
	}}}}

	Refresh(ctx, repo, brk, &st, boardAt, nil)

	v := st.View()
	if cn, _ := v["closing_naked"].([]string); len(cn) != 0 {
		t.Fatalf("決済注文が板に残っているのに裸と報告している: %v — 正常な待ちで警報を出すと麻痺する", cn)
	}
}

// 🚨 照会が失敗した銘柄を **どのリストにも入れずに落とさない**。
// 落とすと障害中は「守りが本当に消えていても画面が完全に無言」になり、
// 事故(裸がどこにも出ていなかった)と同じ形に戻る。
// 今朝 09:42 に実際に立花のセッションエラーが出ている。
func TestProtectiveBoardSurfacesSymbolsItCouldNotCheck(t *testing.T) {
	ctx := context.Background()
	var st State
	repo := closingSweepFixture(t, position.StatusOpen)
	brk := &boardStub{err: map[string]error{
		"4704": errors.New("tachibana CLMOrderList: session still inactive after re-login"),
	}}

	Refresh(ctx, repo, brk, &st, boardAt, nil)

	v := st.View()
	un, _ := v["unknown"].([]string)
	if len(un) != 1 || un[0] != "4704" {
		t.Fatalf("unknown=%v — 照会できなかった銘柄が画面から消えている(障害が無言になる)", v["unknown"])
	}
	// 消えた先が unguarded でもないこと(過剰警報にも倒さない)。
	if ug, _ := v["unguarded"].([]string); len(ug) != 0 {
		t.Fatalf("照会失敗を裸として報告している: %v — 分からないことは分からないと出す", ug)
	}
}

// OPEN の建玉に守りが無いのは決済の進行では説明できないので、従来どおり unguarded。
func TestProtectiveBoardKeepsOpenNakedInTheOriginalBucket(t *testing.T) {
	ctx := context.Background()
	var st State
	repo := closingSweepFixture(t, position.StatusOpen)
	brk := &boardStub{orders: map[string][]port.ProtectiveOrderInfo{"4704": nil}}

	Refresh(ctx, repo, brk, &st, boardAt.Add(time.Minute), nil)

	v := st.View()
	if ug, _ := v["unguarded"].([]string); len(ug) != 1 || ug[0] != "4704" {
		t.Fatalf("unguarded=%v — OPEN の裸は従来どおりこちらに出す", v["unguarded"])
	}
	if cn, _ := v["closing_naked"].([]string); len(cn) != 0 {
		t.Fatalf("OPEN を closing_naked に入れている: %v", cn)
	}
}

var _ = clock.JST
