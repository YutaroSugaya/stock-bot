package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/testutil"
)

// stubKlineBroker is a paper broker whose GetKlines returns canned bars.
type stubKlineBroker struct {
	*broker.Paper
	klines []market.Candle
}

func (s stubKlineBroker) GetKlines(context.Context, string, port.KlinePeriod, int) ([]market.Candle, error) {
	return s.klines, nil
}

func newStubKlineBroker(klines []market.Candle) stubKlineBroker {
	return stubKlineBroker{Paper: broker.NewPaper(clock.System(), 0, 0), klines: klines}
}

// 立花の日足は **分割未調整**: 無検査で upsert すると分割日に見かけ -80% のバーが
// 入り day-horizon 戦略が毒される(CSV 側で実際に 35 件混入)。
func TestRefreshDailyCandlesRejectsSplitDiscontinuity(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryCandleRepo()
	day := func(d int, c float64) market.Candle {
		return market.Candle{OpenTime: time.Date(2026, 6, d, 0, 0, 0, 0, clock.JST), Open: c, High: c, Low: c, Close: c, Volume: 1}
	}
	logger := testutil.SilentLogger()
	if err := repo.Upsert(ctx, "7203", []market.Candle{day(19, 500), day(22, 496)}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 分割未調整(5.75x)のバー → 捨てられること。
	bad := newStubKlineBroker([]market.Candle{day(23, 2854)})
	if n := refreshDailyCandles(ctx, repo, bad, []string{"7203"}, "", logger); n != 0 {
		t.Fatalf("断裂するリフレッシュが適用された (n=%d)", n)
	}
	got, err := repo.List(ctx, "7203", port.PeriodDaily, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("既存バーが汚染された: %+v", got)
	}

	// 通常の値動きは通すこと。
	ok := newStubKlineBroker([]market.Candle{day(23, 510)})
	if n := refreshDailyCandles(ctx, repo, ok, []string{"7203"}, "", logger); n != 1 {
		t.Fatalf("通常のリフレッシュが拒否された (n=%d)", n)
	}
}

// repo が読めないときは **取り込まない**: 履歴と突き合わせできないバーを入れると
// 分割未調整を検出できないまま毒が入る。
func TestRefreshDailyCandlesFailsClosedOnRepoError(t *testing.T) {
	ctx := context.Background()
	bar := market.Candle{OpenTime: time.Date(2026, 6, 23, 0, 0, 0, 0, clock.JST), Close: 100, Open: 100, High: 100, Low: 100, Volume: 1}
	repo := failingListRepo{InMemoryCandleRepo: repository.NewInMemoryCandleRepo()}
	n := refreshDailyCandles(ctx, repo, newStubKlineBroker([]market.Candle{bar}), []string{"7203"}, "",
		testutil.SilentLogger())
	if n != 0 {
		t.Fatalf("履歴を読めないのに取り込んだ (n=%d) — fail-close でない", n)
	}
}

// 同じ日の水準が桁違い(調整済み vs 未調整)なら取り込まない。TZ 正規化が無いと
// この検査は pg が UTC を返す環境で黙って無効化される。
func TestRefreshDailyCandlesRejectsSameDayLevelMismatch(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryCandleRepo()
	logger := testutil.SilentLogger()
	// 保存側は UTC 表現(pg の TIMESTAMPTZ 相当)、取得側は JST。同じ営業日。
	stored := market.Candle{OpenTime: time.Date(2026, 6, 22, 6, 0, 0, 0, time.UTC), Close: 500, Open: 500, High: 500, Low: 500, Volume: 1}
	if err := repo.Upsert(ctx, "7203", []market.Candle{stored}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fetched := market.Candle{OpenTime: time.Date(2026, 6, 22, 0, 0, 0, 0, clock.JST), Close: 2500, Open: 2500, High: 2500, Low: 2500, Volume: 1}
	if n := refreshDailyCandles(ctx, repo, newStubKlineBroker([]market.Candle{fetched}), []string{"7203"}, "", logger); n != 0 {
		t.Fatalf("同日 5倍の水準差を取り込んだ (n=%d)", n)
	}
}

// countingKlineBroker は GetKlines の呼び出し回数(= 立花 API を叩いた回数)を数える。
// 立花には 1 日の API 利用回数の上限があるので、呼び出し回数そのものが検査対象。
type countingKlineBroker struct {
	stubKlineBroker
	calls int
}

func (c *countingKlineBroker) GetKlines(ctx context.Context, s string, p port.KlinePeriod, n int) ([]market.Candle, error) {
	c.calls++
	return c.stubKlineBroker.GetKlines(ctx, s, p, n)
}

func newCountingKlineBroker(klines []market.Candle) *countingKlineBroker {
	return &countingKlineBroker{stubKlineBroker: newStubKlineBroker(klines)}
}

// 既に最新の日足を持っている銘柄は **叩かない**(再起動のたびに 222 銘柄を再取得
// すると無駄が積み上がる)。
func TestRefreshDailyCandlesSkipsSymbolsAlreadyCurrent(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryCandleRepo()
	logger := testutil.SilentLogger()
	bar := market.Candle{OpenTime: time.Date(2026, 6, 23, 0, 0, 0, 0, clock.JST), Open: 100, High: 100, Low: 100, Close: 100, Volume: 1}
	if err := repo.Upsert(ctx, "7203", []market.Candle{bar}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	brk := newCountingKlineBroker([]market.Candle{bar})
	if n := refreshDailyCandles(ctx, repo, brk, []string{"7203"}, "2026-06-23", logger); n != 0 {
		t.Fatalf("最新を持っているのにリフレッシュした (n=%d)", n)
	}
	if brk.calls != 0 {
		t.Fatalf("API を %d 回叩いた — 最新を持つ銘柄はゼロ回であるべき", brk.calls)
	}
}

// 一日でも遅れていれば取りに行く(節約が stale を招いてはならない)。
func TestRefreshDailyCandlesFetchesWhenBehind(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryCandleRepo()
	logger := testutil.SilentLogger()
	day := func(d int) market.Candle {
		return market.Candle{OpenTime: time.Date(2026, 6, d, 0, 0, 0, 0, clock.JST), Open: 100, High: 100, Low: 100, Close: 100, Volume: 1}
	}
	if err := repo.Upsert(ctx, "7203", []market.Candle{day(22)}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	brk := newCountingKlineBroker([]market.Candle{day(22), day(23)})
	if n := refreshDailyCandles(ctx, repo, brk, []string{"7203"}, "2026-06-23", logger); n != 1 {
		t.Fatalf("遅れているのに取りに行かなかった (n=%d)", n)
	}
	if brk.calls != 1 {
		t.Fatalf("calls=%d, want 1", brk.calls)
	}
}

// freshThrough が空(= 最新営業日が分からない)なら従来どおり必ず取りに行く。
// 節約側に倒して古いバーで戦略を回すより、余分に1回叩く方がまし。
func TestRefreshDailyCandlesFetchesWhenFreshnessUnknown(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryCandleRepo()
	bar := market.Candle{OpenTime: time.Date(2026, 6, 23, 0, 0, 0, 0, clock.JST), Open: 100, High: 100, Low: 100, Close: 100, Volume: 1}
	if err := repo.Upsert(ctx, "7203", []market.Candle{bar}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	brk := newCountingKlineBroker([]market.Candle{bar})
	refreshDailyCandles(ctx, repo, brk, []string{"7203"}, "", testutil.SilentLogger())
	if brk.calls != 1 {
		t.Fatalf("calls=%d — 鮮度不明なら取りに行くべき", brk.calls)
	}
}

// 立花は当日の日足を引け後すぐには配信しない(実測: 18:50 時点で最新は
// 前営業日)。全銘柄を引いて初めて「無い」と分かる作りだと、推奨窓の毎時ティックで
// 222 銘柄 × 9 回 ≈ 2,000 リクエストの空振りになる。
func TestRefreshDailyCandlesAbortsRoundWhenBrokerHasNoNewBar(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryCandleRepo()
	logger := testutil.SilentLogger()
	day := func(d int) market.Candle {
		return market.Candle{OpenTime: time.Date(2026, 6, d, 0, 0, 0, 0, clock.JST), Open: 100, High: 100, Low: 100, Close: 100, Volume: 1}
	}
	syms := []string{}
	for i := 0; i < 50; i++ {
		s := fmt.Sprintf("s%02d", i)
		syms = append(syms, s)
		if err := repo.Upsert(ctx, s, []market.Candle{day(22)}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// broker は 06-22 までしか持っていない = 当日分が未配信。
	brk := newCountingKlineBroker([]market.Candle{day(22)})

	if n := refreshDailyCandles(ctx, repo, brk, syms, "2026-06-23", logger); n != 0 {
		t.Fatalf("新しいバーが無いのに %d 銘柄を更新扱いにした", n)
	}
	if brk.calls > maxDailyCanaryProbes {
		t.Fatalf("空振りに %d リクエスト使った — カナリア %d 本で打ち切るべき", brk.calls, maxDailyCanaryProbes)
	}
}

// 節約が「取り込まれない」に化けないこと。
func TestRefreshDailyCandlesProceedsOnceBrokerHasTheBar(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryCandleRepo()
	logger := testutil.SilentLogger()
	day := func(d int) market.Candle {
		return market.Candle{OpenTime: time.Date(2026, 6, d, 0, 0, 0, 0, clock.JST), Open: 100, High: 100, Low: 100, Close: 100, Volume: 1}
	}
	syms := []string{}
	for i := 0; i < 10; i++ {
		s := fmt.Sprintf("s%02d", i)
		syms = append(syms, s)
		if err := repo.Upsert(ctx, s, []market.Candle{day(22)}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	brk := newCountingKlineBroker([]market.Candle{day(22), day(23)}) // 当日分あり

	if n := refreshDailyCandles(ctx, repo, brk, syms, "2026-06-23", logger); n != 10 {
		t.Fatalf("配信済みなのに %d 銘柄しか更新しなかった", n)
	}
}

// failingListRepo は List だけ失敗する repo(Upsert は通る)。
type failingListRepo struct{ *repository.InMemoryCandleRepo }

func (failingListRepo) List(context.Context, string, port.KlinePeriod, int) ([]market.Candle, error) {
	return nil, errListDown
}

var errListDown = errors.New("candle repo down")
