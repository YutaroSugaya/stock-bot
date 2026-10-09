package repository

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

func TestPositionRepo_ClaimForCloseCAS(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryPositionRepo()
	id, _ := r.Insert(ctx, port.PositionInsertInput{Symbol: "7203", Side: order.SideBuy, Quantity: 100, OpenedAt: time.Now()})

	ok, _ := r.ClaimForClose(ctx, id, time.Now())
	if !ok {
		t.Fatal("first claim should succeed")
	}
	ok, _ = r.ClaimForClose(ctx, id, time.Now())
	if ok {
		t.Fatal("second claim should be a benign skip (no double-close)")
	}
}

func TestCloser_AtomicCloseAndRecord(t *testing.T) {
	ctx := context.Background()
	pos := NewInMemoryPositionRepo()
	trades := NewInMemoryTradeRepo()
	closer := NewCloser(pos, trades)

	id, _ := pos.Insert(ctx, port.PositionInsertInput{Symbol: "7203", Side: order.SideBuy, Quantity: 100, OpenedAt: time.Now()})

	// cannot close before claiming
	ok, _ := closer.CloseAndRecord(ctx, id, time.Now(), port.TradeRecord{})
	if ok {
		t.Fatal("close before claim should be ok=false")
	}
	// claim then close
	_, _ = pos.ClaimForClose(ctx, id, time.Now())
	ok, err := closer.CloseAndRecord(ctx, id, time.Now(), port.TradeRecord{Symbol: "7203", ProfitLossJPY: 500, ClosedAt: time.Now()})
	if !ok || err != nil {
		t.Fatalf("close after claim: ok=%v err=%v", ok, err)
	}
	// double close
	ok, _ = closer.CloseAndRecord(ctx, id, time.Now(), port.TradeRecord{})
	if ok {
		t.Fatal("double close should be ok=false")
	}
}

func TestTradeRepo_PerSymbolIsolation(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryTradeRepo()
	now := time.Now()
	_, _ = r.Insert(ctx, port.TradeRecord{Symbol: "7203", ProfitLossJPY: -1000, ClosedAt: now})
	_, _ = r.Insert(ctx, port.TradeRecord{Symbol: "6758", ProfitLossJPY: -3000, ClosedAt: now})

	loss7203, _ := r.SumClosedLossJPYSinceBySymbol(ctx, "7203", now.Add(-time.Hour))
	if loss7203 != 1000 {
		t.Fatalf("7203 loss = %d, want 1000 (not polluted by 6758)", loss7203)
	}
	accountLoss, _ := r.SumClosedLossJPYSince(ctx, now.Add(-time.Hour))
	if accountLoss != 4000 {
		t.Fatalf("account loss = %d, want 4000", accountLoss)
	}
}

func TestTradeRepo_DailyLossIsNetOfCosts(t *testing.T) {
	// Daily loss = NET (gross - fee + carry): fees deepen a loss so the cap
	// trips EARLIER than gross — keep in sync with the pg TradeRepo SQL.
	ctx := context.Background()
	r := NewInMemoryTradeRepo()
	now := time.Now()
	// gross -1000, fee 200, carry -50 → net -1250
	_, _ = r.Insert(ctx, port.TradeRecord{Symbol: "7203", ProfitLossJPY: -1000, FeeJPY: 200, CarryJPY: -50, ClosedAt: now})
	// gross +100 but fee 200 → net -100: a fee-driven loss still counts
	_, _ = r.Insert(ctx, port.TradeRecord{Symbol: "7203", ProfitLossJPY: 100, FeeJPY: 200, ClosedAt: now})
	// clear net winner → not counted
	_, _ = r.Insert(ctx, port.TradeRecord{Symbol: "7203", ProfitLossJPY: 1000, FeeJPY: 200, ClosedAt: now})

	loss, _ := r.SumClosedLossJPYSinceBySymbol(ctx, "7203", now.Add(-time.Hour))
	if loss != 1350 {
		t.Fatalf("net daily loss = %d, want 1350 (1250 + 100)", loss)
	}
}

// forward 検証の読み出し。closed_at 昇順・
// since フィルタ。pg TradeRepo と挙動を揃える(keep in sync)。
func TestTradeRepo_ListClosedSince(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryTradeRepo()
	t0 := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	// 挿入順は closed_at 順とは限らない(reconcile の遅延 close 等)。
	_, _ = r.Insert(ctx, port.TradeRecord{Symbol: "6758", ProfitLossJPY: 200, ClosedAt: t0.Add(2 * time.Hour)})
	_, _ = r.Insert(ctx, port.TradeRecord{Symbol: "7203", ProfitLossJPY: -100, ClosedAt: t0})
	_, _ = r.Insert(ctx, port.TradeRecord{Symbol: "8306", ProfitLossJPY: 300, ClosedAt: t0.Add(-time.Hour)}) // since より前

	got, err := r.ListClosedSince(ctx, t0)
	if err != nil {
		t.Fatalf("ListClosedSince: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (since より前は含めない)", len(got))
	}
	if got[0].Symbol != "7203" || got[1].Symbol != "6758" {
		t.Fatalf("closed_at 昇順でない: %+v", got)
	}
}

func TestTradeRepo_ConsecutiveLosses(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryTradeRepo()
	now := time.Now()
	_, _ = r.Insert(ctx, port.TradeRecord{Symbol: "7203", ProfitLossJPY: 100, ClosedAt: now})
	_, _ = r.Insert(ctx, port.TradeRecord{Symbol: "7203", ProfitLossJPY: -100, ClosedAt: now})
	_, _ = r.Insert(ctx, port.TradeRecord{Symbol: "7203", ProfitLossJPY: -100, ClosedAt: now})
	streak, _ := r.ConsecutiveLossesBySymbol(ctx, "7203")
	if streak != 2 {
		t.Fatalf("streak = %d, want 2", streak)
	}
}

// 起動時の紙帳簿の復元は「config の symbols に今も載っている銘柄」ではなく
// **台帳にある OPEN 全部**を対象にする(ユニバースから外れた銘柄の建玉を
// 取りこぼすと、その建玉は永久に決済できない)。
func TestInMemoryPositionRepoListsOpenAcrossSymbols(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryPositionRepo()
	ins := func(sym string) int64 {
		id, err := r.Insert(ctx, port.PositionInsertInput{Symbol: sym, Side: order.SideBuy, Quantity: 100, OpenedAt: time.Now()})
		if err != nil {
			t.Fatalf("Insert %s: %v", sym, err)
		}
		return id
	}
	id1 := ins("7203")
	id2 := ins("6758")
	closed := ins("8306")
	if _, err := r.ClaimForClose(ctx, closed, time.Now()); err != nil {
		t.Fatalf("ClaimForClose: %v", err)
	}
	if err := r.MarkClosed(ctx, closed, time.Now()); err != nil {
		t.Fatalf("MarkClosed: %v", err)
	}

	got, err := r.ListOpenAllSymbols(ctx)
	if err != nil {
		t.Fatalf("ListOpenAllSymbols: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (CLOSED は含めない): %+v", len(got), got)
	}
	if got[0].ID != id1 || got[1].ID != id2 {
		t.Fatalf("id 昇順でない: %+v", got)
	}
}

// 🛑 二重壁の第2壁は「target が **実データの DSN** と一致しないこと」。target 自身を
// protected に渡すと必ず一致して落ちる — add-migration の rollback 手順
// (STOCKBOT_DATABASE_URL に *_backtest を入れて make migrate-up)が**構造的に実行不能**
// だった。target と protected は別物であることを固定する。
func TestSafeBacktestDSN_AllowsBacktestTargetWhenProtectedDiffers(t *testing.T) {
	target := "postgres://u:p@localhost:5434/stockbot_backtest?sslmode=disable"
	live := "postgres://u:p@localhost:5434/stockbot_live?sslmode=disable"

	if err := SafeBacktestDSN(target, live); err != nil {
		t.Fatalf("_backtest 宛て + 別の実 DSN なら通ること: %v", err)
	}
	if err := SafeBacktestDSN(target, live, target); err == nil {
		t.Fatal("target を protected に混ぜたら落ちる(呼び手が自分自身を渡してはいけない)")
	}
	if err := SafeBacktestDSN(live, target); err == nil {
		t.Fatal("_backtest で終わらない db 名は拒否すること")
	}
}

// 🚨 **人間の売買が bot の再入場ゲートを緩めてはいけない**。
// external_close(人間が証券アプリで建てた建玉の決済・migration 0012)を台帳に書くように
// したのは口座実額を残すためだが、per-symbol の**緩む側**のゲートまで動かしてしまうと
// 「人間が勝った」という事実で bot の連敗キャップとクールダウンが解除される。
//
// 🛑 entry_compensated は**外さない** — あれは bot 自身の往復で、ゲートを締める側に
// 効かせるために台帳へ書いた(migration 0011)。
func TestInMemoryTradeRepo_EntryGatesIgnoreExternalCloses(t *testing.T) {
	ctx := context.Background()
	r := NewInMemoryTradeRepo()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	ins := func(pnl float64, reason string, at time.Time) {
		if _, err := r.Insert(ctx, port.TradeRecord{
			Symbol: "4704", Quantity: 100, EntryPrice: 5000, ClosePrice: 5000,
			ProfitLossJPY: pnl, CloseReason: reason, ClosedAt: at,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	ins(-500, "stop_loss", base)                                       // bot の負け
	ins(-400, port.CloseReasonEntryCompensated, base.Add(time.Minute)) // bot の巻き戻し(数える)
	ins(+9000, port.CloseReasonExternalClose, base.Add(2*time.Minute)) // 人間の勝ち(数えない)

	n, err := r.ConsecutiveLossesBySymbol(ctx, "4704")
	if err != nil {
		t.Fatalf("consecutive: %v", err)
	}
	if n != 2 {
		t.Fatalf("連敗 = %d, want 2 — 人間の勝ちで bot の連敗が切れている", n)
	}

	last, err := r.LastCloseBySymbol(ctx, "4704")
	if err != nil || last == nil {
		t.Fatalf("last close: %v %+v", err, last)
	}
	if last.NetJPY >= 0 {
		t.Fatalf("直近決済 net = %v — 人間の利確を拾うと cooldown が after_loss から外れる", last.NetJPY)
	}

	cnt, err := r.CountTradesSinceBySymbol(ctx, "4704", base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("窓内の取引数 = %d, want 2(人間の分は数えない)", cnt)
	}

	// 🛑 口座に効いた実額(日次損失)は**両方とも数える** — 委託保証金・維持率に効く。
	loss, err := r.SumClosedLossJPYSinceBySymbol(ctx, "4704", base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("loss: %v", err)
	}
	if loss != 900 {
		t.Fatalf("日次損失 = %d, want 900(-500 と -400。人間の +9000 は損失ではない)", loss)
	}
}
