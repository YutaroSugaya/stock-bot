package query

import (
	"context"
	"math"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

// mustReport は「since ゼロで成功する」呼び出し。同じ 4 行が 13 箇所にあった。
// 🛑 since 非ゼロのテスト(since が repo に渡ることを見る)と error 経路のテスト
// (エラーになること自体が検査対象)は**これを使わない** — 使うと検査対象が消える。
func mustReport(t *testing.T, repo port.TradeRepository, opts ...ForwardReportOption) ForwardReportView {
	t.Helper()
	rep, err := NewBuildForwardReport(repo, opts...).Execute(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return rep
}

// BuildForwardReport は ListClosedSince しか呼ばない。残りは埋め込みの nil のままで、
// 呼ばれたら panic する = 「読み取り専用の 1 本しか触っていない」ことを型で保つ。
type fakeTradeRepo struct {
	port.TradeRepository
	trades   []port.TradeRecord
	gotSince time.Time
}

func (f *fakeTradeRepo) ListClosedSince(_ context.Context, since time.Time) ([]port.TradeRecord, error) {
	f.gotSince = since
	return f.trades, nil
}

func jstAt(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, clock.JST)
}

type fakeStrategyResolver struct{ names map[int64]string }

func (f *fakeStrategyResolver) StrategyByPositionID(_ context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	for _, id := range ids {
		if n, ok := f.names[id]; ok {
			out[id] = n
		}
	}
	return out, nil
}

// net は gross − fee + carry(daily loss cap と同じ規約)。月次は JST。
func TestForwardReportAggregatesNetOfCosts(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{Symbol: "6758", Side: "BUY", Quantity: 100, ProfitLossJPY: 2000, FeeJPY: 300, CarryJPY: -100, CloseReason: "take_profit", ClosedAt: jstAt(2026, 8, 3, 10)},
		{Symbol: "7203", Side: "BUY", Quantity: 100, ProfitLossJPY: -1000, FeeJPY: 200, CarryJPY: 0, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 7, 27, 14)},
		{Symbol: "7203", Side: "BUY", Quantity: 100, ProfitLossJPY: 500, FeeJPY: 600, CarryJPY: 0, CloseReason: "max_hold", ClosedAt: jstAt(2026, 7, 29, 14)},
	}}
	rep := mustReport(t, repo)
	if rep.N != 3 {
		t.Fatalf("N = %d, want 3", rep.N)
	}
	// net: -1200, -100, +1600 → wins は net>0 の 1 本だけ(gross +500 は fee 負け)
	if rep.Wins != 1 {
		t.Fatalf("Wins = %d, want 1 (gross 勝ち・net 負けを勝ちに数えない)", rep.Wins)
	}
	if rep.NetTotalJPY != 300 {
		t.Fatalf("NetTotal = %v, want 300 (=-1200-100+1600)", rep.NetTotalJPY)
	}
	if rep.SymbolN != 2 {
		t.Fatalf("SymbolN = %d, want 2", rep.SymbolN)
	}
	if len(rep.NetPnLJPY) != 3 || rep.NetPnLJPY[0] != -1200 || rep.NetPnLJPY[1] != -100 || rep.NetPnLJPY[2] != 1600 {
		t.Fatalf("NetPnLJPY 系列が closed_at 昇順の net でない: %v", rep.NetPnLJPY)
	}
	if len(rep.ByMonth) != 2 || rep.ByMonth[0].Month != "2026-07" || rep.ByMonth[0].N != 2 || rep.ByMonth[0].NetJPY != -1300 ||
		rep.ByMonth[1].Month != "2026-08" || rep.ByMonth[1].NetJPY != 1600 {
		t.Fatalf("月次集計が壊れている: %+v", rep.ByMonth)
	}
	if rep.ByReason["stop_loss"] != 1 || rep.ByReason["take_profit"] != 1 || rep.ByReason["max_hold"] != 1 {
		t.Fatalf("close_reason 集計が壊れている: %+v", rep.ByReason)
	}
	if len(rep.Trades) != 3 || rep.Trades[0].Symbol != "7203" || rep.Trades[0].NetJPY != -1200 {
		t.Fatalf("Trades ビューが closed_at 昇順でない: %+v", rep.Trades)
	}
}

func TestForwardReportPassesSinceAndFormatsIt(t *testing.T) {
	repo := &fakeTradeRepo{}
	since := jstAt(2026, 7, 24, 0)
	rep, err := NewBuildForwardReport(repo).Execute(context.Background(), since)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !repo.gotSince.Equal(since) {
		t.Fatalf("since が repo に渡っていない: %v", repo.gotSince)
	}
	if rep.Since != "2026-07-24" {
		t.Fatalf("Since = %q, want 2026-07-24", rep.Since)
	}
}

func TestForwardReportEmpty(t *testing.T) {
	rep := mustReport(t, &fakeTradeRepo{})
	if rep.N != 0 || rep.Wins != 0 || rep.NetTotalJPY != 0 || len(rep.NetPnLJPY) != 0 || rep.Since != "" {
		t.Fatalf("空レポートが壊れている: %+v", rep)
	}
}

// advisor は複数戦略を建てるので、戦略別分計が forward 検証の本体。
func TestForwardReportBreaksDownByStrategy(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", ProfitLossJPY: 3000, FeeJPY: 500, CloseReason: "take_profit", ClosedAt: jstAt(2026, 7, 27, 10)},
		{PositionID: 2, Symbol: "6758", ProfitLossJPY: -1000, FeeJPY: 200, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 7, 28, 10)},
		{PositionID: 3, Symbol: "8306", ProfitLossJPY: 2000, FeeJPY: 300, CloseReason: "max_hold", ClosedAt: jstAt(2026, 7, 29, 10)},
		{PositionID: 9, Symbol: "9984", ProfitLossJPY: 100, FeeJPY: 0, CloseReason: "max_hold", ClosedAt: jstAt(2026, 7, 30, 10)}, // resolver が知らない ID
	}}
	res := &fakeStrategyResolver{names: map[int64]string{1: "bnf_reversion", 2: "high_volume_premium", 3: "bnf_reversion"}}

	rep := mustReport(t, repo, WithStrategyResolver(res))
	if len(rep.ByStrategy) != 3 {
		t.Fatalf("ByStrategy = %+v, want 3 rows (bnf_reversion/high_volume_premium/unknown)", rep.ByStrategy)
	}
	// net 降順: bnf_reversion +4200 (2500+1700) / unknown +100 / high_volume_premium -1200
	top := rep.ByStrategy[0]
	if top.Strategy != "bnf_reversion" || top.N != 2 || top.Wins != 2 || top.NetJPY != 4200 {
		t.Fatalf("bnf 集計が壊れている: %+v", top)
	}
	if rep.ByStrategy[2].Strategy != "high_volume_premium" || rep.ByStrategy[2].NetJPY != -1200 {
		t.Fatalf("net 降順でない: %+v", rep.ByStrategy)
	}
	if rep.ByStrategy[1].Strategy != "unknown" {
		t.Fatalf("resolver が知らない ID は unknown に落とす: %+v", rep.ByStrategy[1])
	}
	if rep.Trades[0].Strategy != "bnf_reversion" {
		t.Fatalf("Trades 行にも戦略名を出す: %+v", rep.Trades[0])
	}
}

func TestForwardReportFiltersByStrategy(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", ProfitLossJPY: 3000, FeeJPY: 500, CloseReason: "take_profit", ClosedAt: jstAt(2026, 7, 27, 10)},
		{PositionID: 2, Symbol: "6758", ProfitLossJPY: -1000, FeeJPY: 200, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 7, 28, 10)},
	}}
	res := &fakeStrategyResolver{names: map[int64]string{1: "bnf_reversion", 2: "high_volume_premium"}}

	rep := mustReport(t, repo, WithStrategyResolver(res), WithStrategyFilter("bnf_reversion"))
	if rep.N != 1 || len(rep.NetPnLJPY) != 1 || rep.NetPnLJPY[0] != 2500 || rep.NetTotalJPY != 2500 {
		t.Fatalf("フィルタ後の集計が全体と混ざっている: %+v", rep)
	}
	if len(rep.ByStrategy) != 1 || rep.ByStrategy[0].Strategy != "bnf_reversion" {
		t.Fatalf("フィルタ後の ByStrategy: %+v", rep.ByStrategy)
	}
}

// 黙って全件を返すのではなく fail-close。
func TestForwardReportFilterRequiresResolver(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", ProfitLossJPY: 1000, ClosedAt: jstAt(2026, 7, 27, 10)},
	}}
	_, err := NewBuildForwardReport(repo, WithStrategyFilter("bnf_reversion")).Execute(context.Background(), time.Time{})
	if err == nil {
		t.Fatal("resolver 無しのフィルタは error にする(黙って別物を返さない)")
	}
}

func TestForwardReportWithoutResolver(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", ProfitLossJPY: 1000, ClosedAt: jstAt(2026, 7, 27, 10)},
	}}
	rep := mustReport(t, repo)
	if rep.N != 1 || len(rep.ByStrategy) != 0 {
		t.Fatalf("resolver 無しの後方互換が壊れている: %+v", rep)
	}
}

// JST 日付・新しい日が先(上から読む順)。
func TestForwardReportBreaksDownByDay(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{Symbol: "7203", ProfitLossJPY: 1000, FeeJPY: 200, CloseReason: "take_profit", ClosedAt: jstAt(2026, 7, 27, 10)},
		{Symbol: "6758", ProfitLossJPY: 500, FeeJPY: 600, CloseReason: "max_hold", ClosedAt: jstAt(2026, 7, 27, 14)},
		{Symbol: "7203", ProfitLossJPY: -1000, FeeJPY: 200, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 7, 28, 10)},
	}}
	rep := mustReport(t, repo)
	if len(rep.ByDay) != 2 {
		t.Fatalf("ByDay = %+v, want 2 rows", rep.ByDay)
	}
	// 新しい日が先: 07-28 (-1200, 0勝) → 07-27 (+800-100=+700, 1勝2本)
	if rep.ByDay[0].Day != "2026-07-28" || rep.ByDay[0].N != 1 || rep.ByDay[0].Wins != 0 || rep.ByDay[0].NetJPY != -1200 {
		t.Fatalf("ByDay[0] (新しい日が先) = %+v", rep.ByDay[0])
	}
	if rep.ByDay[1].Day != "2026-07-27" || rep.ByDay[1].N != 2 || rep.ByDay[1].Wins != 1 || rep.ByDay[1].NetJPY != 700 {
		t.Fatalf("ByDay[1] = %+v", rep.ByDay[1])
	}
}

// net 降順・同額は銘柄コード昇順。
func TestForwardReportBreaksDownBySymbol(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{Symbol: "7203", ProfitLossJPY: 1000, FeeJPY: 200, CloseReason: "take_profit", ClosedAt: jstAt(2026, 7, 27, 10)},
		{Symbol: "6758", ProfitLossJPY: -1000, FeeJPY: 200, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 7, 27, 14)},
		{Symbol: "7203", ProfitLossJPY: 500, FeeJPY: 100, CloseReason: "max_hold", ClosedAt: jstAt(2026, 7, 28, 10)},
	}}
	rep := mustReport(t, repo)
	if len(rep.BySymbol) != 2 {
		t.Fatalf("BySymbol = %+v, want 2 rows", rep.BySymbol)
	}
	if rep.BySymbol[0].Symbol != "7203" || rep.BySymbol[0].N != 2 || rep.BySymbol[0].Wins != 2 || rep.BySymbol[0].NetJPY != 1200 {
		t.Fatalf("BySymbol[0] (net 降順) = %+v", rep.BySymbol[0])
	}
	if rep.BySymbol[1].Symbol != "6758" || rep.BySymbol[1].NetJPY != -1200 {
		t.Fatalf("BySymbol[1] = %+v", rep.BySymbol[1])
	}
}

// 建玉金額が銘柄で 20倍以上ばらつくので、生の円のまま判定すると「値がさ株を掴んだ
// 戦略」が勝つ。edge-judge は正規化した net_per_1m_jpy を食う。
func TestForwardReportNormalisesTo1MNotional(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		// 建玉 73,200円(9831 相当)で +1,000円 → ¥1M 換算 +13,661円
		{Symbol: "9831", Quantity: 100, EntryPrice: 732, ProfitLossJPY: 1000, CloseReason: "take_profit", ClosedAt: jstAt(2026, 7, 27, 10)},
		// 建玉 1,589,000円(7735 相当)で +1,000円 → ¥1M 換算 +629円(同じ円でも寄与は小さい)
		{Symbol: "7735", Quantity: 100, EntryPrice: 15890, ProfitLossJPY: 1000, CloseReason: "take_profit", ClosedAt: jstAt(2026, 7, 27, 11)},
	}}
	rep := mustReport(t, repo)
	if len(rep.NetPer1MJPY) != 2 {
		t.Fatalf("NetPer1MJPY = %v, want 2 本", rep.NetPer1MJPY)
	}
	want0 := 1000.0 / (732 * 100) * 1e6
	want1 := 1000.0 / (15890 * 100) * 1e6
	if d := rep.NetPer1MJPY[0] - want0; d > 0.01 || d < -0.01 {
		t.Errorf("NetPer1MJPY[0] = %v, want %v", rep.NetPer1MJPY[0], want0)
	}
	if d := rep.NetPer1MJPY[1] - want1; d > 0.01 || d < -0.01 {
		t.Errorf("NetPer1MJPY[1] = %v, want %v", rep.NetPer1MJPY[1], want1)
	}
	// 生の円は同額でも、正規化すると 21.7 倍の差がつく — これが判定に効く。
	if rep.NetPer1MJPY[0] <= rep.NetPer1MJPY[1]*10 {
		t.Errorf("正規化が効いていない: %v vs %v", rep.NetPer1MJPY[0], rep.NetPer1MJPY[1])
	}
	// 合計・戦略別・日別にも正規化 net を併記する。
	if d := rep.NetPer1MTotalJPY - (want0 + want1); d > 0.01 || d < -0.01 {
		t.Errorf("NetPer1MTotalJPY = %v, want %v", rep.NetPer1MTotalJPY, want0+want1)
	}
	if len(rep.ByDay) != 1 || rep.ByDay[0].NetPer1MJPY == 0 {
		t.Errorf("日別に正規化 net が無い: %+v", rep.ByDay)
	}
	if len(rep.BySymbol) != 2 || rep.BySymbol[0].NetPer1MJPY == 0 {
		t.Errorf("銘柄別に正規化 net が無い: %+v", rep.BySymbol)
	}
}

// days は net 系列と**同順同長**でないと judge が落ちる。universe_n は銘柄集中
// ゲートの入力で、symbol_n しか出ていなかったため黙ってスキップされていた。
func TestForwardReportEmitsDaysAndUniverseNForJudge(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{Symbol: "7203", Quantity: 100, EntryPrice: 1000, ProfitLossJPY: 1000, CloseReason: "take_profit", ClosedAt: jstAt(2026, 7, 27, 10)},
		{Symbol: "6758", Quantity: 100, EntryPrice: 1000, ProfitLossJPY: -500, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 7, 27, 14)},
		{Symbol: "7203", Quantity: 100, EntryPrice: 1000, ProfitLossJPY: 200, CloseReason: "max_hold", ClosedAt: jstAt(2026, 7, 29, 10)},
	}}
	rep := mustReport(t, repo)
	want := []string{"2026-07-27", "2026-07-27", "2026-07-29"}
	if len(rep.Days) != len(want) {
		t.Fatalf("Days = %v, want %v", rep.Days, want)
	}
	for i := range want {
		if rep.Days[i] != want[i] {
			t.Fatalf("Days[%d] = %q, want %q (net 系列と同順)", i, rep.Days[i], want[i])
		}
	}
	if len(rep.Days) != len(rep.NetPer1MJPY) || len(rep.Days) != len(rep.NetPnLJPY) {
		t.Fatalf("Days/net 系列の長さが揃っていない: %d / %d / %d", len(rep.Days), len(rep.NetPer1MJPY), len(rep.NetPnLJPY))
	}
	if rep.UniverseN != rep.SymbolN || rep.UniverseN != 2 {
		t.Fatalf("UniverseN = %d, want 2 (= SymbolN %d)", rep.UniverseN, rep.SymbolN)
	}
}

// ¥1M 正規化は「単位あたりの強さ」しか測らないので、「その株を実際に買えるか」は
// 建玉金額で切って別に見る。**診断であって昇格ゲートではない**。
func TestForwardReportFiltersByMaxNotional(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		// 建玉 400万円 — 90万円の枠では買えない
		{PositionID: 1, Symbol: "285A", Quantity: 100, EntryPrice: 40000, ProfitLossJPY: 50000, CloseReason: "take_profit", ClosedAt: jstAt(2026, 7, 27, 10)},
		// 建玉 80万円 — 買える
		{PositionID: 2, Symbol: "7203", Quantity: 100, EntryPrice: 8000, ProfitLossJPY: -2000, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 7, 28, 10)},
		// 建玉 ちょうど 90万円 — 境界は含む(<=)
		{PositionID: 3, Symbol: "6758", Quantity: 100, EntryPrice: 9000, ProfitLossJPY: 1000, CloseReason: "max_hold", ClosedAt: jstAt(2026, 7, 29, 10)},
	}}
	rep := mustReport(t, repo, WithMaxNotionalJPY(900000))
	if rep.N != 2 || rep.NetTotalJPY != -1000 {
		t.Fatalf("閾値で絞れていない: N=%d net=%v (want 2 / -1000)", rep.N, rep.NetTotalJPY)
	}
	if rep.MaxNotionalJPY != 900000 {
		t.Fatalf("MaxNotionalJPY = %v, want 900000(絞った事実を出力に残す)", rep.MaxNotionalJPY)
	}
	// 系列の同期が保たれる(judge は days と net を同順同長で食う)。
	if len(rep.NetPer1MJPY) != 2 || len(rep.Days) != 2 || len(rep.NetPnLJPY) != 2 {
		t.Fatalf("系列の同期が崩れた: net1m=%d days=%d net=%d", len(rep.NetPer1MJPY), len(rep.Days), len(rep.NetPnLJPY))
	}
	if rep.Days[0] != "2026-07-28" || rep.Days[1] != "2026-07-29" {
		t.Fatalf("Days = %v, 除外された 07-27 が残っている", rep.Days)
	}
	if rep.UniverseN != 2 {
		t.Fatalf("UniverseN = %d, want 2(絞った後の銘柄数)", rep.UniverseN)
	}
}

func TestForwardReportMaxNotionalCombinesWithStrategyFilter(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "285A", Quantity: 100, EntryPrice: 40000, ProfitLossJPY: 50000, CloseReason: "take_profit", ClosedAt: jstAt(2026, 7, 27, 10)},
		{PositionID: 2, Symbol: "7203", Quantity: 100, EntryPrice: 8000, ProfitLossJPY: -2000, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 7, 28, 10)},
		{PositionID: 3, Symbol: "6758", Quantity: 100, EntryPrice: 5000, ProfitLossJPY: 700, CloseReason: "max_hold", ClosedAt: jstAt(2026, 7, 29, 10)},
	}}
	res := &fakeStrategyResolver{names: map[int64]string{1: "atr_breakout", 2: "atr_breakout", 3: "abs_momentum"}}
	rep := mustReport(t, repo, WithStrategyResolver(res), WithStrategyFilter("atr_breakout"), WithMaxNotionalJPY(900000))
	if rep.N != 1 || rep.NetTotalJPY != -2000 {
		t.Fatalf("併用が効いていない: N=%d net=%v (want 1 / -2000)", rep.N, rep.NetTotalJPY)
	}
	if len(rep.ByStrategy) != 1 || rep.ByStrategy[0].Strategy != "atr_breakout" {
		t.Fatalf("ByStrategy = %+v", rep.ByStrategy)
	}
}

// notional 不明を黙って落とすと「小資金でも成立」を過大に見せる方向に効く。
func TestForwardReportMaxNotionalKeepsUnknownNotional(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{Symbol: "X", Quantity: 0, EntryPrice: 0, ProfitLossJPY: 500, CloseReason: "manual", ClosedAt: jstAt(2026, 7, 27, 10)},
	}}
	rep := mustReport(t, repo, WithMaxNotionalJPY(900000))
	if rep.N != 1 {
		t.Fatalf("notional 不明の行を閾値で落とした: N=%d", rep.N)
	}
}

// map に無い ID = 復元不能。
type fakeScoreResolver struct{ scores map[int64]port.TradeScore }

func (f *fakeScoreResolver) ScoreByPositionID(_ context.Context, ids []int64) (map[int64]port.TradeScore, error) {
	out := make(map[int64]port.TradeScore, len(ids))
	for _, id := range ids {
		if s, ok := f.scores[id]; ok {
			out[id] = s
		}
	}
	return out, nil
}

// score は戦略ごとに定義が違い**横断比較してはいけない**(abs_momentum は上限
// なし、donchian は 1.0 付近が上限)ので、分位は必ず**戦略内**で切る。
func TestForwardReportScoreQuantilesWithinStrategy(t *testing.T) {
	// entry 10000 × qty 100 = ¥1M notional → net1m == net(検算しやすい)。
	mk := func(id int64, net float64, day int) port.TradeRecord {
		return port.TradeRecord{PositionID: id, Symbol: "7203", Side: "BUY", Quantity: 100,
			EntryPrice: 10000, ProfitLossJPY: net, CloseReason: "stop_loss", ClosedAt: jstAt(2026, 7, day, 10)}
	}
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		mk(1, -100, 27), mk(2, -200, 27), mk(3, 300, 28), mk(4, 400, 28), mk(5, 0, 29),
	}}
	strat := &fakeStrategyResolver{names: map[int64]string{
		1: "abs_momentum", 2: "abs_momentum", 3: "abs_momentum", 4: "abs_momentum", 5: "abs_momentum",
	}}
	scores := &fakeScoreResolver{scores: map[int64]port.TradeScore{
		1: {Score: 1.0}, 2: {Score: 1.1}, 3: {Score: 1.3}, 4: {Score: 1.4},
	}}
	rep := mustReport(t, repo, WithStrategyResolver(strat), WithScoreResolver(scores))
	if rep.ScoreMissingN != 1 {
		t.Fatalf("ScoreMissingN = %d, want 1(黙って落とさない)", rep.ScoreMissingN)
	}
	if len(rep.ByStrategy) != 1 || rep.ByStrategy[0].Score == nil {
		t.Fatalf("戦略別 score 分計が無い: %+v", rep.ByStrategy)
	}
	sv := rep.ByStrategy[0].Score
	if sv.Restored != 4 || sv.Missing != 1 {
		t.Fatalf("Restored/Missing = %d/%d, want 4/1", sv.Restored, sv.Missing)
	}
	if math.Abs(sv.Median-1.2) > 1e-9 { // (1.1+1.3)/2
		t.Fatalf("Median = %v, want 1.2", sv.Median)
	}
	// 低スコア半分 = {1.0, 1.1} → avg(-100,-200) = -150。高スコア半分 = {1.3, 1.4} → +350。
	if sv.LowN != 2 || sv.LowAvgPer1MJPY != -150 || sv.HighN != 2 || sv.HighAvgPer1MJPY != 350 {
		t.Fatalf("分位分計が違う: %+v", sv)
	}
	// 奇数 n の中央の1本は Low 側(参照実測と同じ割り付け規約)。
	odd := buildScoreView([]scoredTrade{{1.0, 10}, {1.2, 20}, {1.4, 30}}, 0)
	if odd.LowN != 2 || odd.HighN != 1 || odd.Median != 1.2 {
		t.Fatalf("奇数分割の規約が違う: %+v", odd)
	}
	var withScore, without int
	for _, tr := range rep.Trades {
		if tr.Score != nil {
			withScore++
		} else {
			without++
		}
	}
	if withScore != 4 || without != 1 {
		t.Fatalf("per-trade score = %d/%d, want 4/1", withScore, without)
	}
}

// 系列長を保たないと判定の N が実際と食い違う。
func TestForwardReportNormalisationHandlesMissingNotional(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{Symbol: "X", Quantity: 0, EntryPrice: 0, ProfitLossJPY: 500, CloseReason: "manual", ClosedAt: jstAt(2026, 7, 27, 10)},
	}}
	rep := mustReport(t, repo)
	if len(rep.NetPer1MJPY) != 1 || rep.NetPer1MJPY[0] != 0 {
		t.Fatalf("NetPer1MJPY = %v, want [0]", rep.NetPer1MJPY)
	}
}

// 🛑 `entry_compensated`(約定した後に守りを board に置けず entry saga が巻き戻した往復)は
// **戦略の出口ではない**。エッジの分子にも分母にも入れてはいけない — 入れると
// 「戦略が 5 回試して負けた」に見えるが、実際には戦略の出口は 1 度も発火していない。
// live で同一銘柄を 5 往復した事故がこれ。
//
// ただし **黙って落とさない**: 何件除外したかを必ず出す(台帳から消えたのか、
// そもそも起きなかったのかを画面で区別できるようにする)。
func TestForwardReportExcludesCompensatedRoundTripsButCountsThem(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", Quantity: 100, EntryPrice: 1000, ProfitLossJPY: 500, CloseReason: "take_profit", ClosedAt: jstAt(2026, 8, 18, 10)},
		{PositionID: 2, Symbol: "4704", Quantity: 100, EntryPrice: 5351, ProfitLossJPY: 600, CloseReason: "entry_compensated", ClosedAt: jstAt(2026, 8, 18, 9)},
		{PositionID: 3, Symbol: "4704", Quantity: 100, EntryPrice: 5399, ProfitLossJPY: -700, CloseReason: "entry_compensated", ClosedAt: jstAt(2026, 8, 18, 9)},
	}}

	rep := mustReport(t, repo)

	if rep.N != 1 {
		t.Fatalf("N = %d, want 1 — 巻き戻した往復がエッジ標本に混ざっている", rep.N)
	}
	if rep.NetTotalJPY != 500 {
		t.Fatalf("NetTotalJPY = %v, want 500", rep.NetTotalJPY)
	}
	if rep.SymbolN != 1 {
		t.Fatalf("SymbolN = %d, want 1(4704 は戦略の標本ではない)", rep.SymbolN)
	}
	if rep.CompensatedN != 2 {
		t.Fatalf("CompensatedN = %d, want 2 — 除外件数を黙って捨てている", rep.CompensatedN)
	}
	if _, ok := rep.ByReason["entry_compensated"]; ok {
		t.Fatalf("ByReason に entry_compensated が残っている: %v", rep.ByReason)
	}
}

// external(人間が建てた)建玉の決済も **戦略の出口ではない**のでエッジ標本から外す。
// 口座には実額として効いているので件数は残す(migration 0012)。
func TestForwardReportExcludesExternalCloses(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", Quantity: 100, EntryPrice: 1000, ProfitLossJPY: 500, CloseReason: "take_profit", ClosedAt: jstAt(2026, 8, 18, 10)},
		{PositionID: 2, Symbol: "6758", Quantity: 100, EntryPrice: 2500, ProfitLossJPY: 10000, CloseReason: "external_close", ClosedAt: jstAt(2026, 8, 18, 11)},
	}}

	rep := mustReport(t, repo)

	if rep.N != 1 || rep.NetTotalJPY != 500 {
		t.Fatalf("N=%d net=%v, want 1/500 — 人間の建玉がエッジ標本に混ざっている", rep.N, rep.NetTotalJPY)
	}
	if rep.ExternalN != 1 {
		t.Fatalf("ExternalN = %d, want 1 — 除外件数を黙って捨てている", rep.ExternalN)
	}
}

// 🛑 件数だけでは画面が「何も無かった」と読める。live がその状態で、
// `/api/live/performance` は n:0 / compensated_n:5 を正直に返していたのに、
// ダッシュボードは n しか描かないので **戦績が消えたように見えた**。
// 「黙って落とさない」を成立させるには **net も** 出す必要がある(口座には実額で効いている)。
func TestForwardReportReportsNonStrategyNet(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", Quantity: 100, EntryPrice: 1000, ProfitLossJPY: 500, CloseReason: "take_profit", ClosedAt: jstAt(2026, 8, 18, 10)},
		{PositionID: 2, Symbol: "4704", Quantity: 100, EntryPrice: 5351, ProfitLossJPY: 600, FeeJPY: 100, CloseReason: "entry_compensated", ClosedAt: jstAt(2026, 8, 18, 9)},
		{PositionID: 3, Symbol: "4704", Quantity: 100, EntryPrice: 5399, ProfitLossJPY: -700, CarryJPY: -50, CloseReason: "entry_compensated", ClosedAt: jstAt(2026, 8, 18, 9)},
		{PositionID: 4, Symbol: "6758", Quantity: 100, EntryPrice: 2500, ProfitLossJPY: 10000, FeeJPY: 200, CloseReason: "external_close", ClosedAt: jstAt(2026, 8, 18, 11)},
	}}

	rep := mustReport(t, repo)

	// gross 600 − fee 100 + carry 0、gross −700 − fee 0 + carry −50 = 500 − 750
	if rep.CompensatedNetJPY != -250 {
		t.Fatalf("CompensatedNetJPY = %v, want -250 — 除外した往復の net を黙って捨てている", rep.CompensatedNetJPY)
	}
	if rep.ExternalNetJPY != 9800 {
		t.Fatalf("ExternalNetJPY = %v, want 9800 — 人間の建玉の net を黙って捨てている", rep.ExternalNetJPY)
	}
	// 除外した net が戦績側へ漏れていないこと(両方向を固定する)。
	if rep.NetTotalJPY != 500 {
		t.Fatalf("NetTotalJPY = %v, want 500 — 戦略外の net が戦績に混ざっている", rep.NetTotalJPY)
	}
}

// 🛑 **画面(戦績タブ)は口座ベースで数える**。
// `entry_compensated` / `external_close` を「戦績の外の別枠カード」に出す UI は廃止し、
// **普通のトレードとして戦略に計上する** — 全件が巻き戻しだった日に戦績が空に見え、
// 口座で実際に動いた実額が画面から消える(live がその状態だった)。
//
// **エッジ標本(cmd/forward-report → cmd/edge-judge)は既定のまま除外**を維持する
// (事前コミット)。どちらの数え方かは `counting` が名乗り、
// 取り違えは cmd/edge-judge が fail-close で弾く。
func TestForwardReportCountsNonStrategyClosesWhenAsked(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", Quantity: 100, EntryPrice: 1000, ProfitLossJPY: 500, CloseReason: "take_profit", ClosedAt: jstAt(2026, 8, 18, 10)},
		{PositionID: 2, Symbol: "4704", Quantity: 100, EntryPrice: 5399, ProfitLossJPY: -700, FeeJPY: 100, CloseReason: "entry_compensated", ClosedAt: jstAt(2026, 8, 18, 9)},
		{PositionID: 3, Symbol: "6758", Quantity: 100, EntryPrice: 2500, ProfitLossJPY: 10000, FeeJPY: 200, CloseReason: "external_close", ClosedAt: jstAt(2026, 8, 18, 11)},
	}}
	resolver := &fakeStrategyResolver{names: map[int64]string{1: "abs_momentum_v2", 2: "bnf_reversion"}}

	rep := mustReport(t, repo, WithStrategyResolver(resolver), WithNonStrategyClosesCounted())

	if rep.Counting != CountingAccount {
		t.Fatalf("Counting = %q, want %q — どちらの数え方か名乗らない出力は edge-judge に混ざる", rep.Counting, CountingAccount)
	}
	if rep.N != 3 || rep.Wins != 2 {
		t.Fatalf("N=%d Wins=%d, want 3/2 — 戦略外の決済を計上していない", rep.N, rep.Wins)
	}
	if rep.NetTotalJPY != 9500 { // 500 + (-800) + 9800
		t.Fatalf("NetTotalJPY = %v, want 9500(口座に効いた実額)", rep.NetTotalJPY)
	}
	if rep.SymbolN != 3 || rep.UniverseN != 3 {
		t.Fatalf("SymbolN=%d UniverseN=%d, want 3/3", rep.SymbolN, rep.UniverseN)
	}
	for _, r := range []string{"take_profit", "entry_compensated", "external_close"} {
		if rep.ByReason[r] != 1 {
			t.Fatalf("ByReason[%q] = %d, want 1: %v", r, rep.ByReason[r], rep.ByReason)
		}
	}
	// 系列(判定入力と同形)も同順同長で伸びる — 片方だけ足す差分を作らせない。
	if len(rep.NetPnLJPY) != 3 || len(rep.NetPer1MJPY) != 3 || len(rep.Days) != 3 || len(rep.Trades) != 3 {
		t.Fatalf("系列長 net=%d per1m=%d days=%d trades=%d, want 全部 3",
			len(rep.NetPnLJPY), len(rep.NetPer1MJPY), len(rep.Days), len(rep.Trades))
	}
	// 巻き戻した往復は **建てた戦略の行**に乗る(position に config が凍結してある)。
	byStrat := map[string]ForwardStrategyView{}
	for _, s := range rep.ByStrategy {
		byStrat[s.Strategy] = s
	}
	if s := byStrat["bnf_reversion"]; s.N != 1 || s.NetJPY != -800 {
		t.Fatalf("ByStrategy[bnf_reversion] = %+v, want N=1 net=-800 — 巻き戻した往復が戦略に計上されていない", s)
	}
	// 人間の建玉は戦略が引けない。**別枠に逃がさず** unknown 行として同じ表に出す。
	if s := byStrat["unknown"]; s.N != 1 || s.NetJPY != 9800 {
		t.Fatalf("ByStrategy[unknown] = %+v, want N=1 net=9800", s)
	}
	// 内数としての件数・net は残す(何本が戦略の出口でなかったかは消さない)。
	if rep.CompensatedN != 1 || rep.ExternalN != 1 {
		t.Fatalf("CompensatedN=%d ExternalN=%d, want 1/1 — 内訳を黙って捨てている", rep.CompensatedN, rep.ExternalN)
	}
}

// 既定(cmd/forward-report → edge-judge の経路)は**今までどおり除外**。
// 名乗りも edge_sample のまま — 事前コミットを画面の都合で動かさない。
func TestForwardReportDefaultsToEdgeSampleCounting(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", Quantity: 100, EntryPrice: 1000, ProfitLossJPY: 500, CloseReason: "take_profit", ClosedAt: jstAt(2026, 8, 18, 10)},
		{PositionID: 2, Symbol: "4704", Quantity: 100, EntryPrice: 5399, ProfitLossJPY: -700, CloseReason: "entry_compensated", ClosedAt: jstAt(2026, 8, 18, 9)},
	}}

	rep := mustReport(t, repo)

	if rep.Counting != CountingEdgeSample {
		t.Fatalf("Counting = %q, want %q", rep.Counting, CountingEdgeSample)
	}
	if rep.N != 1 || len(rep.NetPnLJPY) != 1 {
		t.Fatalf("N=%d 系列=%d, want 1/1 — 既定の除外が壊れている", rep.N, len(rep.NetPnLJPY))
	}
}

// 🚨 事前登録は締めの判定材料を **3 つ**挙げている:
//
//  1. 保有期間の分布(戦略 × 決済理由)      … cmd/holding-period が担う
//  2. **max_hold 決済の本数と net(戦略別)** … どこにも無かった
//  3. 反実仮想(キャップが無ければどうだったか)… cmd/counterfactual が担う
//
// 2 だけ集計する場所が無く、トップレベルの `by_reason` は**全戦略合算の件数だけ**
// だった(net も戦略の別も無い)。つまり「MaxHold を半分にしたのは効いたのか、
// 刈りすぎたのか」という中心の問いに、道具が答えられない状態だった。
func TestForwardReportBreaksDownEachStrategyByCloseReason(t *testing.T) {
	repo := &fakeTradeRepo{trades: []port.TradeRecord{
		{PositionID: 1, Symbol: "7203", ProfitLossJPY: 3000, FeeJPY: 500, CloseReason: "take_profit", ClosedAt: jstAt(2026, 7, 27, 10)},
		{PositionID: 3, Symbol: "8306", ProfitLossJPY: 2000, FeeJPY: 300, CloseReason: "max_hold", ClosedAt: jstAt(2026, 7, 29, 10)},
		{PositionID: 4, Symbol: "9432", ProfitLossJPY: -800, FeeJPY: 200, CloseReason: "max_hold", ClosedAt: jstAt(2026, 7, 30, 10)},
	}}
	res := &fakeStrategyResolver{names: map[int64]string{1: "bnf_reversion", 3: "bnf_reversion", 4: "bnf_reversion"}}

	rep := mustReport(t, repo, WithStrategyResolver(res))
	if len(rep.ByStrategy) != 1 {
		t.Fatalf("前提: 1 戦略にまとまること: %+v", rep.ByStrategy)
	}
	var mh *ForwardReasonView
	for i := range rep.ByStrategy[0].ByReason {
		if rep.ByStrategy[0].ByReason[i].Reason == "max_hold" {
			mh = &rep.ByStrategy[0].ByReason[i]
		}
	}
	if mh == nil {
		t.Fatalf("戦略別の決済理由内訳が無い — 判定材料 2 が出せない: %+v", rep.ByStrategy[0])
	}
	// max_hold: +1700(2000-300)と -1000(-800-200)= net +700 / 2本 / 勝ち1本
	if mh.N != 2 || mh.Wins != 1 || mh.NetJPY != 700 {
		t.Fatalf("max_hold の内訳が合わない: %+v", *mh)
	}
	// 他の理由も同じ粒度で出る(max_hold だけ特別扱いしない)。
	found := false
	for _, r := range rep.ByStrategy[0].ByReason {
		if r.Reason == "take_profit" && r.N == 1 && r.NetJPY == 2500 {
			found = true
		}
	}
	if !found {
		t.Fatalf("take_profit の内訳が無い/合わない: %+v", rep.ByStrategy[0].ByReason)
	}
}
