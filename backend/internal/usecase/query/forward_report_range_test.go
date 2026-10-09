package query

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/port"
)

// 画面の戦績はサイクル(期間)単位で切る。
// 同じ DB に複数の期間が入っていると、開始日だけでは
// 最初の期間を切り出せない — 終了日(その日の 0 時 JST・排他)も要る。
func TestForwardReportExecuteRangeExcludesTradesAtOrAfterUntil(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{Symbol: "7203", Side: "BUY", Quantity: 100, ProfitLossJPY: 1000, CloseReason: "take_profit", ClosedAt: jstAt(2026, 8, 7, 14)},
		{Symbol: "6758", Side: "BUY", Quantity: 100, ProfitLossJPY: -500, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 8, 8, 0)},
		{Symbol: "9984", Side: "BUY", Quantity: 100, ProfitLossJPY: 300, CloseReason: "max_hold", ClosedAt: jstAt(2026, 8, 12, 10)},
	}}
	since := jstAt(2026, 7, 28, 0)
	until := jstAt(2026, 8, 8, 0)
	rep, err := NewBuildForwardReport(repo).ExecuteRange(context.Background(), since, until)
	if err != nil {
		t.Fatalf("ExecuteRange: %v", err)
	}
	if !repo.gotSince.Equal(since) {
		t.Fatalf("repo に渡した since = %v, want %v", repo.gotSince, since)
	}
	if rep.N != 1 || rep.NetTotalJPY != 1000 {
		t.Fatalf("N=%d net=%v, want N=1 net=1000(until ちょうど以降は除外)", rep.N, rep.NetTotalJPY)
	}
	if rep.Since != "2026-07-28" || rep.Until != "2026-08-08" {
		t.Fatalf("since/until = %q/%q, want 2026-07-28/2026-08-08(適用中の期間を応答から出す)", rep.Since, rep.Until)
	}
}

// until ゼロは上限なし(従来の Execute と同じ)。
func TestForwardReportExecuteRangeZeroUntilIsOpenEnded(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{Symbol: "7203", Side: "BUY", Quantity: 100, ProfitLossJPY: 1000, CloseReason: "take_profit", ClosedAt: jstAt(2026, 9, 14, 10)},
	}}
	rep, err := NewBuildForwardReport(repo).ExecuteRange(context.Background(), time.Time{}, time.Time{})
	if err != nil || rep.N != 1 || rep.Until != "" {
		t.Fatalf("err=%v N=%d until=%q, want N=1 until=\"\"", err, rep.N, rep.Until)
	}
}
