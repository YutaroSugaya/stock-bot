package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/safety"
	"stockbot/backend/internal/usecase/command"
)

// hotPanicDaily builds a daily history whose LAST bar (yesterday, JST) is a BNF
// panic — the Tier A candidate condition (TACHIBANA_API_NOTES §1.6-1 の解消)。
func hotPanicDaily(sym string, today time.Time, n int, panic bool) []market.Candle {
	y, m, d := today.In(clock.JST).Date()
	day0 := time.Date(y, m, d, 0, 0, 0, 0, clock.JST).AddDate(0, 0, -n)
	cs := make([]market.Candle, 0, n)
	for i := 0; i < n; i++ {
		c := market.Candle{
			Symbol: sym, OpenTime: day0.AddDate(0, 0, i), Interval: 24 * time.Hour,
			Open: 1000, High: 1005, Low: 995, Close: 1000, Volume: 1000,
		}
		if panic && i == n-1 {
			c.Open, c.High, c.Low, c.Close, c.Volume = 940, 950, 855, 860, 5000
		}
		cs = append(cs, c)
	}
	return cs
}

func hotTestBundle(t *testing.T, sym string, daily []market.Candle, at time.Time, hw *HotWatch) *SymbolBundle {
	t.Helper()
	repo := repository.NewInMemoryCandleRepo()
	if err := repo.Upsert(context.Background(), sym, daily); err != nil {
		t.Fatal(err)
	}
	pb := broker.NewPaper(clock.Fixed(at), 0, 0)
	pb.SetPrice(sym, 860)
	c := clock.Fixed(at)
	posRepo := repository.NewInMemoryPositionRepo()
	closer := repository.NewCloser(posRepo, repository.NewInMemoryTradeRepo())
	es := safety.NewEmergencyStop(filepath.Join(t.TempDir(), "emg.flag"), nil)
	return &SymbolBundle{
		Symbol:        sym,
		Broker:        pb,
		Candles:       repo,
		Manage:        command.NewManageOpenPositions(pb, posRepo, closer, es, c, 0.30),
		Flatten:       command.NewForceFlatten(pb, posRepo, closer, es, tokyoHours(), c),
		Hours:         tokyoHours(),
		Clock:         c,
		Counters:      &Counters{},
		Agg:           market.NewAggregator(sym, 64),
		Configs:       NewConfigSet(nil), // no_trade 相当(armed でない)
		PosRepo:       posRepo,
		MonitorNarrow: true,
		HotWatch:      hw,
	}
}

// 前日パニックの銘柄は PriceTick が HotWatch に hot 登録し、方式B の
// 「armed でも建玉でもない銘柄は poll しない」も**突破して**ポーリングされる —
// バックテストが測る対象(パニック翌日の分足)は arm の有無と無関係に記録が要るため。
func TestPriceTick_HotCandidateIsWatchedDespiteMonitorNarrow(t *testing.T) {
	at := time.Date(2026, 7, 30, 10, 0, 0, 0, clock.JST) // Thu 10:00 JST in-session
	hw := NewHotWatch()
	b := hotTestBundle(t, "7203", hotPanicDaily("7203", at, 40, true), at, hw)

	b.PriceTick(context.Background())

	if !hw.IsHot("7203") {
		t.Fatal("前日パニック銘柄が HotWatch に登録されていない")
	}
	if got := b.Counters.PriceTicks.Load(); got != 1 {
		t.Fatalf("hot 候補が MonitorNarrow に skip された: PriceTicks=%d want 1", got)
	}
}

// 平常銘柄は hot にならず、方式B の skip も従来どおり効く。
func TestPriceTick_ColdSymbolStaysNarrow(t *testing.T) {
	at := time.Date(2026, 7, 30, 10, 0, 0, 0, clock.JST)
	hw := NewHotWatch()
	b := hotTestBundle(t, "6758", hotPanicDaily("6758", at, 40, false), at, hw)

	b.PriceTick(context.Background())

	if hw.IsHot("6758") {
		t.Fatal("平常銘柄が hot 登録されている")
	}
	if got := b.Counters.PriceTicks.Load(); got != 0 {
		t.Fatalf("方式B の skip が壊れた: PriceTicks=%d want 0", got)
	}
}

// 日をまたいだら判定し直す(前日の hot が翌日に持ち越されない)。
func TestHotWatch_ReevaluatesOnNewDay(t *testing.T) {
	day1 := time.Date(2026, 7, 30, 10, 0, 0, 0, clock.JST)
	hw := NewHotWatch()
	// day1: パニック前日 → hot。
	b := hotTestBundle(t, "7203", hotPanicDaily("7203", day1, 40, true), day1, hw)
	b.PriceTick(context.Background())
	if !hw.IsHot("7203") {
		t.Fatal("day1 に hot になっていない")
	}
	// day2(翌営業日): 同じ日足のままだと「パニックは2日前」— 前日確定バーは
	// 平常なので hot が落ちる。
	day2 := time.Date(2026, 7, 31, 10, 0, 0, 0, clock.JST)
	extended := append(hotPanicDaily("7203", day1, 40, true), market.Candle{
		Symbol: "7203", OpenTime: time.Date(2026, 7, 30, 0, 0, 0, 0, clock.JST),
		Interval: 24 * time.Hour, Open: 860, High: 900, Low: 855, Close: 880, Volume: 2000,
	})
	b2 := hotTestBundle(t, "7203", extended, day2, hw)
	b2.PriceTick(context.Background())
	if hw.IsHot("7203") {
		t.Fatal("翌日に hot が持ち越されている(前日確定バーは平常)")
	}
}
