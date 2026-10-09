package query_test

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/query"
)

type fakeJournalSource struct {
	trades     []port.JournalTrade
	opened     []port.JournalPosition
	open       []port.JournalPosition
	rejections []port.JournalRejection
	screens    []port.JournalScreen
	from, to   time.Time
}

func (f *fakeJournalSource) JournalClosedTrades(_ context.Context, from, to time.Time) ([]port.JournalTrade, error) {
	f.from, f.to = from, to
	return f.trades, nil
}
func (f *fakeJournalSource) JournalOpenedPositions(context.Context, time.Time, time.Time) ([]port.JournalPosition, error) {
	return f.opened, nil
}
func (f *fakeJournalSource) JournalOpenPositionsAsOf(context.Context, time.Time) ([]port.JournalPosition, error) {
	return f.open, nil
}
func (f *fakeJournalSource) JournalRejections(context.Context, time.Time, time.Time) ([]port.JournalRejection, error) {
	return f.rejections, nil
}
func (f *fakeJournalSource) JournalScreens(context.Context, time.Time, time.Time) ([]port.JournalScreen, error) {
	return f.screens, nil
}

func day(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, clock.JST)
}

// 段1 は **数字だけ**を決定論で組む(解釈は段2 の LLM で、しかも入力はこの JSON のみ)。
// net は台帳の規約どおり gross − fee + carry。
func TestBuildDailyJournal_AggregatesTheDay(t *testing.T) {
	src := &fakeJournalSource{
		trades: []port.JournalTrade{
			{Symbol: "7203", Strategy: "abs_momentum_v2", Quantity: 100, EntryPrice: 1000, ClosePrice: 1030,
				GrossJPY: 3000, FeeJPY: 100, CarryJPY: -20, CloseReason: "take_profit", ClosedAt: day(2026, 8, 14, 10)},
			{Symbol: "6501", Strategy: "abs_momentum_v2", Quantity: 100, EntryPrice: 2000, ClosePrice: 1900,
				GrossJPY: -10000, FeeJPY: 100, CarryJPY: -30, CloseReason: "stop_loss", ClosedAt: day(2026, 8, 14, 11)},
			{Symbol: "9984", Strategy: "donchian_breakout_v2", Quantity: 100, EntryPrice: 5000, ClosePrice: 5000,
				GrossJPY: 0, FeeJPY: 0, CarryJPY: 0, CloseReason: "max_hold", ClosedAt: day(2026, 8, 14, 15)},
		},
		opened: []port.JournalPosition{{Symbol: "4704", Strategy: "bnf_reversion", Quantity: 100, EntryPrice: 6965, OpenedAt: day(2026, 8, 14, 9)}},
		open:   []port.JournalPosition{{Symbol: "4704", Strategy: "bnf_reversion", Quantity: 100, EntryPrice: 6965, OpenedAt: day(2026, 8, 12, 9)}},
		rejections: []port.JournalRejection{
			{Reason: "nanpin_blocked", N: 12}, {Reason: "collateral_insufficient", N: 3},
		},
		screens: []port.JournalScreen{{Strategy: "abs_momentum_v2", Triggered: 22, Picked: 2, TopSymbols: []string{"7203", "6501"}}},
	}

	got, err := query.NewBuildDailyJournal(src).Execute(context.Background(), day(2026, 8, 14, 0))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got.Date != "2026-08-14" {
		t.Fatalf("Date = %q", got.Date)
	}
	// 期間は JST 日の半開区間 [00:00, 翌00:00)。ここがずれると「当日の記録」が別日を混ぜる。
	if !src.from.Equal(day(2026, 8, 14, 0)) || !src.to.Equal(day(2026, 8, 15, 0)) {
		t.Fatalf("期間 = [%v, %v)", src.from, src.to)
	}
	if got.Trades.N != 3 || got.Trades.Wins != 1 || got.Trades.Losses != 1 || got.Trades.Flat != 1 {
		t.Fatalf("勝敗 = %+v", got.Trades)
	}
	// gross 3000-10000+0 = -7000 / fee 200 / carry -50 → net = -7000-200+(-50) = -7250
	if got.Trades.GrossJPY != -7000 || got.Trades.FeeJPY != 200 || got.Trades.CarryJPY != -50 || got.Trades.NetJPY != -7250 {
		t.Fatalf("net の内訳 = %+v", got.Trades)
	}
	if n := got.Trades.ByReason["stop_loss"]; n != 1 {
		t.Fatalf("理由別 = %+v", got.Trades.ByReason)
	}
	byStrat := map[string]query.JournalStrategyStat{}
	for _, s := range got.Trades.ByStrategy {
		byStrat[s.Strategy] = s
	}
	if s := byStrat["abs_momentum_v2"]; s.N != 2 || s.NetJPY != -7250 {
		t.Fatalf("戦略別 = %+v", got.Trades.ByStrategy)
	}
	// 戦略別は N 降順 → 同数なら戦略名昇順(日ごとに順序が揺れると差分が読めない)。
	if got.Trades.ByStrategy[0].Strategy != "abs_momentum_v2" {
		t.Fatalf("戦略別の並び = %+v", got.Trades.ByStrategy)
	}
	if len(got.Entries) != 1 || got.Entries[0].Symbol != "4704" {
		t.Fatalf("entries = %+v", got.Entries)
	}
	if got.Open.N != 1 || got.Open.Positions[0].HeldDays != 2 {
		t.Fatalf("open = %+v(保有日数は暦日)", got.Open)
	}
	if got.Rejections[0].Reason != "nanpin_blocked" || got.Rejections[0].N != 12 {
		t.Fatalf("rejections = %+v(N 降順)", got.Rejections)
	}
	if got.Screens[0].Triggered != 22 {
		t.Fatalf("screens = %+v", got.Screens)
	}
}

// 何も起きなかった日でも**空の総評を書ける**(0 と「データ無し」を混同しない)。
func TestBuildDailyJournal_QuietDay(t *testing.T) {
	got, err := query.NewBuildDailyJournal(&fakeJournalSource{}).Execute(context.Background(), day(2026, 8, 14, 0))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.Trades.N != 0 || got.Trades.NetJPY != 0 {
		t.Fatalf("静かな日 = %+v", got.Trades)
	}
	if got.Entries == nil || got.Rejections == nil || got.Screens == nil {
		t.Fatal("空配列は null ではなく [] で出す(段2 の入力が壊れる)")
	}
}

// 🛑 引け後パケットは **段2(LLM)への入力**。ここは**口座ベースで数える**。
// `entry_compensated`(約定後に守りを置けず巻き戻した往復)
// も `external_close`(人間が建てた建玉の決済)も**普通のトレードとして戦略に計上する** —
// 別枠に逃がすと、全件がそれだった日に「今日は何も無かった」と読める総評になる
// (live がその状態だった)。**何がどう閉じたかは by_reason と closed[] に
// 残る**ので、段2 は「戦略の出口だったか」をそこから読む。
//
// エッジ標本(cmd/forward-report → cmd/edge-judge)は別で、そちらは除外を維持する。
func TestBuildDailyJournal_CountsNonStrategyClosesAsTrades(t *testing.T) {
	src := &fakeJournalSource{
		trades: []port.JournalTrade{
			{Symbol: "7203", Strategy: "abs_momentum_v2", Quantity: 100, EntryPrice: 1000, ClosePrice: 1030,
				GrossJPY: 3000, FeeJPY: 100, CarryJPY: 0, CloseReason: "take_profit", ClosedAt: day(2026, 8, 18, 10)},
			{Symbol: "4704", Strategy: "bnf_reversion", Quantity: 100, EntryPrice: 5399, ClosePrice: 5392,
				GrossJPY: -700, FeeJPY: 0, CarryJPY: 0, CloseReason: "entry_compensated", ClosedAt: day(2026, 8, 18, 9)},
			{Symbol: "6758", Strategy: "", Quantity: 100, EntryPrice: 2500, ClosePrice: 2600,
				GrossJPY: 10000, FeeJPY: 0, CarryJPY: 0, CloseReason: "external_close", ClosedAt: day(2026, 8, 18, 11)},
		},
	}

	got, err := query.NewBuildDailyJournal(src).Execute(context.Background(), day(2026, 8, 18, 0))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got.Trades.N != 3 || got.Trades.Wins != 2 || got.Trades.Losses != 1 {
		t.Fatalf("N=%d W=%d L=%d, want 3/2/1 — 戦略外の決済を戦績に計上していない", got.Trades.N, got.Trades.Wins, got.Trades.Losses)
	}
	if got.Trades.NetJPY != 12200 { // (3000-100) + (-700) + 10000
		t.Fatalf("NetJPY = %v, want 12200(口座に効いた実額)", got.Trades.NetJPY)
	}
	// 巻き戻した往復は**建てた戦略の行**に乗る。戦略が引けない人間の建玉は unknown 行。
	byStrat := map[string]query.JournalStrategyStat{}
	for _, s := range got.Trades.ByStrategy {
		byStrat[s.Strategy] = s
	}
	if s := byStrat["bnf_reversion"]; s.N != 1 || s.NetJPY != -700 {
		t.Fatalf("ByStrategy[bnf_reversion] = %+v, want N=1 net=-700", s)
	}
	if s := byStrat["unknown"]; s.N != 1 || s.NetJPY != 10000 {
		t.Fatalf("ByStrategy[unknown] = %+v, want N=1 net=10000(空文字の戦略名を出さない)", s)
	}
	// 何がどう閉じたかは消さない — 段2 はここを読んで「戦略の出口だったか」を書く。
	for _, r := range []string{"take_profit", "entry_compensated", "external_close"} {
		if got.Trades.ByReason[r] != 1 {
			t.Fatalf("ByReason[%q] = %d, want 1: %v", r, got.Trades.ByReason[r], got.Trades.ByReason)
		}
	}
	if len(got.Trades.Trades) != 3 {
		t.Fatalf("closed[] = %d 本, want 3", len(got.Trades.Trades))
	}
}
