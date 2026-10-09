package app

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// `AdvisorCycle.Run`(LLM)を決定論の関数へ差し替える。
// `universe()` → `RankCandidates()` → `selectAdvisePicks(nil)` → `Arm()` は元から決定論で、
// 差し替えるのは 1 点だけ。

type llmCallCounter struct{ calls int }

func (a *llmCallCounter) Generate(context.Context, *market.MarketSummary) (*port.AdvisorRun, error) {
	a.calls++
	return nil, nil
}

func deterministicLoop(t *testing.T, armed *[]*config.StrategyConfig, adv *llmCallCounter, at time.Time) *AdvisorLoop {
	t.Helper()
	repo := repository.NewInMemoryCandleRepo()
	if err := repo.Upsert(context.Background(), "7203", panicSeries("7203")); err != nil {
		t.Fatal(err)
	}
	l := armLoop(repo, []string{"7203"}, adv, armed, at, 10, 10)
	l.BuildConfig = func(sym string, slot config.StrategyName, daily []market.Candle) (*config.StrategyConfig, error) {
		c := &config.StrategyConfig{
			ConfigID: "det-" + sym + "-" + string(slot), Symbol: sym,
			StrategyName: slot, Mode: config.ModePaper, HoldingMode: config.HoldingMultiday,
		}
		c.Entry.Direction = config.DirectionBuyOnly
		c.Risk.Quantity = 100
		return c, nil
	}
	return l
}

// 🛑 BuildConfig があるとき LLM は **1 回も呼ばれない**。ここが漏れると、外したはずの
// 交絡(97% 通過フィルタ)が静かに戻る。
func TestAdvisorLoopDeterministicArmNeverCallsTheLLM(t *testing.T) {
	at := time.Date(2026, 8, 24, 10, 0, 0, 0, clock.JST)
	adv := &llmCallCounter{}
	var armed []*config.StrategyConfig
	deterministicLoop(t, &armed, adv, at).Tick(context.Background())

	if adv.calls != 0 {
		t.Fatalf("LLM が %d 回呼ばれた — 決定論 arm では 0 回", adv.calls)
	}
	if len(armed) == 0 {
		t.Fatal("決定論テンプレートで 1 本も arm されていない")
	}
	for _, c := range armed {
		if c.AdvisorRunID != "" {
			t.Fatalf("advisor 由来の provenance が付いている: %q", c.AdvisorRunID)
		}
	}
}

// 同じ入力 → 同じ arm(再現性)。LLM の「同じ入力でも出力が揺れうる」性質が消える。
func TestAdvisorLoopDeterministicArmIsReproducible(t *testing.T) {
	at := time.Date(2026, 8, 24, 10, 0, 0, 0, clock.JST)
	var a, b []*config.StrategyConfig
	deterministicLoop(t, &a, &llmCallCounter{}, at).Tick(context.Background())
	deterministicLoop(t, &b, &llmCallCounter{}, at).Tick(context.Background())

	if len(a) != len(b) || len(a) == 0 {
		t.Fatalf("arm 数が違う: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ConfigID != b[i].ConfigID {
			t.Fatalf("%d 本目が違う: %q vs %q", i, a[i].ConfigID, b[i].ConfigID)
		}
	}
}

// `screen_snapshots` の書込周期を arm 周期から**分離する**。heartbeat を 1 分に
// すると 1 日 270 ラウンド × 1,600 行 = 432,000 行/日(現在の 45 倍)になるが、
// ランキングは日中不変で毎回ほぼ同一内容なので、分離すれば書込量は増えない。
func TestAdvisorLoopSeparatesSnapshotCadenceFromArmCadence(t *testing.T) {
	at := time.Date(2026, 8, 24, 10, 0, 0, 0, clock.JST)
	now := at
	var armed []*config.StrategyConfig
	rec := &countingScreens{}
	l := deterministicLoop(t, &armed, &llmCallCounter{}, at)
	l.Clock = func() time.Time { return now }
	l.Screens = rec
	l.Heartbeat = time.Minute
	l.SnapshotInterval = time.Hour

	for i := 0; i < 5; i++ {
		l.Tick(context.Background())
		now = now.Add(time.Minute)
	}
	if rec.rounds != 1 {
		t.Fatalf("snapshot 書込 = %d ラウンド, want 1(1時間周期)", rec.rounds)
	}

	now = now.Add(time.Hour)
	l.Tick(context.Background())
	if rec.rounds != 2 {
		t.Fatalf("周期を跨いでも書いていない: %d ラウンド", rec.rounds)
	}
}

type countingScreens struct {
	rounds int
	rows   int
}

func (s *countingScreens) InsertRound(_ context.Context, rows []port.ScreenSnapshot) error {
	s.rounds++
	s.rows += len(rows)
	return nil
}

// 🚨 **周期の内側で picked が変わったら「差分だけ」書く**を production 経路で縛る。
//
// これまでこの不変条件を触るテストは 2 本とも**単体呼び出し**だった
// (`shouldSnapshot` / `newlyPicked` を直接叩くだけ)。`Tick` を回すテストは picks が
// 全ティック不変なので **advisor_loop.go の差分枝を一度も通らず**、
// `persistScreenSnapshots(ctx, now, newlyPicked(...), picks)` を `ranked` に戻す退行が
// **全テスト緑のまま通る**状態だった(1 日 19 万行に戻り、周期分離の目的が消える。
// 敵対的レビューで実際に起きた形)。
func TestPickedChangeWritesOnlyTheDeltaThroughTick(t *testing.T) {
	at := time.Date(2026, 8, 24, 10, 0, 0, 0, clock.JST)
	now := at
	repo := repository.NewInMemoryCandleRepo()
	// 2 銘柄 × 1 戦略。片方を「建玉中」にすると picked 集合が変わる。
	for _, sym := range []string{"7203", "6501"} {
		if err := repo.Upsert(context.Background(), sym, panicSeries(sym)); err != nil {
			t.Fatal(err)
		}
	}
	var armed []*config.StrategyConfig
	l := armLoop(repo, []string{"7203", "6501"}, &llmCallCounter{}, &armed, at, 10, 10)
	l.BuildConfig = func(sym string, slot config.StrategyName, _ []market.Candle) (*config.StrategyConfig, error) {
		c := &config.StrategyConfig{
			ConfigID: "det-" + sym + "-" + string(slot), Symbol: sym,
			StrategyName: slot, Mode: config.ModePaper, HoldingMode: config.HoldingMultiday,
		}
		c.Entry.Direction = config.DirectionBuyOnly
		c.Risk.Quantity = 100
		return c, nil
	}
	rec := &countingScreens{}
	l.Screens = rec
	l.Clock = func() time.Time { return now }
	l.Heartbeat = time.Minute
	l.SnapshotInterval = time.Hour // 周期は跨がせない

	l.Tick(context.Background())
	if rec.rounds != 1 {
		t.Fatalf("初回に全ランキングを書いていない: rounds=%d", rec.rounds)
	}
	full := rec.rows
	if full == 0 {
		t.Fatal("初回の書込が 0 行 — 候補が出ていない(テストの前提が壊れている)")
	}

	// 片方を建玉中にして picked 集合を変える(周期は跨がない)。
	held := map[string]bool{"7203": true}
	l.IsHeld = func(_ context.Context, sym string, _ config.StrategyName) bool { return held[sym] }
	now = now.Add(time.Minute)
	l.Tick(context.Background())

	if rec.rounds != 2 {
		t.Fatalf("picked が変わったのに書いていない: rounds=%d — 周期分離で "+
			"「唯一日中に変わる列」のサンプルが失われる", rec.rounds)
	}
	delta := rec.rows - full
	// 🛑 **差分であること**。全ランキングを書き直すと退行(1日19万行)。
	if delta >= full {
		t.Fatalf("差分 %d 行 >= 全ランキング %d 行 — newlyPicked ではなく ranked を"+
			"書いている(周期分離の目的が消える)", delta, full)
	}
}
