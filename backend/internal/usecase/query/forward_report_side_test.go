package query

import (
	"testing"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 🚨 事前登録した「**売り側だけ**の edge-judge」を回す道具。これが無いと
// 売り側の net 系列を切り出せず、「向きを開けたのが良かったか」を判定できないまま
// 締めを迎える。
func TestForwardReport_SideFilter(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 1000,
			ClosePrice: 1010, ProfitLossJPY: 1000, CloseReason: "take_profit", ClosedAt: jstAt(2026, 8, 24, 15)},
		{PositionID: 2, Symbol: "6501", Side: order.SideBuy, Quantity: 100, EntryPrice: 1000,
			ClosePrice: 996, ProfitLossJPY: -400, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 8, 24, 15)},
		{PositionID: 3, Symbol: "9432", Side: order.SideSell, Quantity: 100, EntryPrice: 1000,
			ClosePrice: 993, ProfitLossJPY: 700, CloseReason: "take_profit", ClosedAt: jstAt(2026, 8, 24, 15)},
	}}

	if got := mustReport(t, repo).N; got != 3 {
		t.Fatalf("全件 = %d, want 3", got)
	}

	sells := mustReport(t, repo, WithSideFilter("sell"))
	if sells.N != 1 {
		t.Fatalf("SELL = %d, want 1", sells.N)
	}
	// 何で絞ったかが出力に残らないと、後から「これは全件だ」と読まれる。
	if sells.SideFilter != "SELL" {
		t.Fatalf("SideFilter = %q, want SELL", sells.SideFilter)
	}
	if sells.NetTotalJPY != 700 {
		t.Fatalf("SELL の net = %v, want 700", sells.NetTotalJPY)
	}

	buys := mustReport(t, repo, WithSideFilter("BUY"))
	if buys.N != 2 {
		t.Fatalf("BUY = %d, want 2", buys.N)
	}
	if buys.SideFilter != "BUY" {
		t.Fatalf("SideFilter = %q", buys.SideFilter)
	}
}

// 🛑 **買いと売りの成績を 1 つの net に混ぜない**。
//
// `direction: both` の 8 アーム(v2 3種 + 52週高値 と、その _trail 兄弟)は
// 売りも建てる。戦略別の net はその両方を合算していたので、
// 「エッジが下落側の流動性供給に固有か」という問い自体が測れない
// (買いで勝って売りで負けていても、合算 net は平らに見える)。
//
// `-side BUY` / `-side SELL` を 2 回打てば分けられるが、**既定の 1 画面で
// 内訳が見えないと、採点者は合算だけ見て verdict を出す**。
func TestForwardReportSplitsStrategyNetBySide(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", Side: "BUY", ProfitLossJPY: 3000, CloseReason: "take_profit", ClosedAt: jstAt(2026, 7, 27, 10)},
		{PositionID: 2, Symbol: "6758", Side: "SELL", ProfitLossJPY: -1000, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 7, 28, 10)},
		{PositionID: 3, Symbol: "8306", Side: "SELL", ProfitLossJPY: -500, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 7, 29, 10)},
	}}
	res := &fakeStrategyResolver{names: map[int64]string{1: "abs_momentum_v2", 2: "abs_momentum_v2", 3: "abs_momentum_v2"}}

	rep := mustReport(t, repo, WithStrategyResolver(res))
	if len(rep.ByStrategy) != 1 {
		t.Fatalf("ByStrategy=%d, want 1", len(rep.ByStrategy))
	}
	g := rep.ByStrategy[0]
	if g.N != 3 || g.NetJPY != 1500 {
		t.Fatalf("合計は従来どおり: %+v", g)
	}
	if g.BuyN != 1 || g.BuyNetJPY != 3000 {
		t.Errorf("買い内訳 = N%d net%.0f, want N1 net3000", g.BuyN, g.BuyNetJPY)
	}
	if g.SellN != 2 || g.SellNetJPY != -1500 {
		t.Errorf("売り内訳 = N%d net%.0f, want N2 net-1500 — 売りの負けが合算に埋もれる", g.SellN, g.SellNetJPY)
	}
}

// 向きが空(旧建玉 / external)を売りに数えない。
func TestForwardReportTreatsUnknownSideAsBuy(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", ProfitLossJPY: 100, CloseReason: "take_profit", ClosedAt: jstAt(2026, 7, 27, 10)},
	}}
	res := &fakeStrategyResolver{names: map[int64]string{1: "bnf_reversion"}}
	rep := mustReport(t, repo, WithStrategyResolver(res))
	if rep.ByStrategy[0].SellN != 0 || rep.ByStrategy[0].BuyN != 1 {
		t.Errorf("Buy=%d Sell=%d, want 1/0", rep.ByStrategy[0].BuyN, rep.ByStrategy[0].SellN)
	}
}
