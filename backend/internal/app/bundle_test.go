package app

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/testutil"
)

// klinesBroker is a minimal port.Broker that only answers GetKlines.
// MOCK rationale (TESTING.md 3用途): §1 system boundary — stands in for the
// broker's klines call; other methods are never reached by dailyCandles.
type klinesBroker struct {
	port.Broker
	candles []market.Candle
}

func (k klinesBroker) GetKlines(context.Context, string, port.KlinePeriod, int) ([]market.Candle, error) {
	return k.candles, nil
}

func dailyBar(symbol string, close float64) market.Candle {
	return market.Candle{Symbol: symbol, Interval: 24 * time.Hour, OpenTime: time.Now().UTC(), Open: close, High: close, Low: close, Close: close}
}

// The daily-history data path must source from the CandleRepository first (the
// 2026 fix: it was wired only to Broker.GetKlines, which returns nil for
// paper, so day-horizon strategies were always starved).
func TestDailyCandles_RepoFirst(t *testing.T) {
	repo := repository.NewInMemoryCandleRepo()
	if err := repo.Upsert(context.Background(), "7203", []market.Candle{dailyBar("7203", 1234)}); err != nil {
		t.Fatal(err)
	}
	b := &SymbolBundle{Symbol: "7203", Candles: repo, Broker: klinesBroker{candles: []market.Candle{dailyBar("7203", 9999)}}}

	got := b.dailyCandles(context.Background())
	if len(got) != 1 || got[0].Close != 1234 {
		t.Fatalf("expected repo candle (1234), got %+v", got)
	}
}

func TestDailyCandles_FallbackToBrokerWhenRepoEmpty(t *testing.T) {
	repo := repository.NewInMemoryCandleRepo() // empty
	b := &SymbolBundle{Symbol: "7203", Candles: repo, Broker: klinesBroker{candles: []market.Candle{dailyBar("7203", 9999)}}}

	got := b.dailyCandles(context.Background())
	if len(got) != 1 || got[0].Close != 9999 {
		t.Fatalf("expected broker fallback candle (9999), got %+v", got)
	}
}

func TestDailyCandles_EmptyWhenNeitherHasData(t *testing.T) {
	repo := repository.NewInMemoryCandleRepo()
	b := &SymbolBundle{Symbol: "7203", Candles: repo, Broker: klinesBroker{candles: nil}}

	if got := b.dailyCandles(context.Background()); len(got) != 0 {
		t.Fatalf("expected no candles, got %+v", got)
	}
}

func dailyBarAt(symbol string, close float64, openTime time.Time) market.Candle {
	return market.Candle{Symbol: symbol, Interval: 24 * time.Hour, OpenTime: openTime, Open: close, High: close, Low: close, Close: close}
}

// A day-horizon strategy must NOT be fed a frozen feed: when the newest daily
// bar is older than the staleness threshold, dailyCandles returns nil (so the
// strategy reports insufficient history) and bumps the StaleDaily counter.
func TestDailyCandles_StaleFeedBlocks(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	repo := repository.NewInMemoryCandleRepo()
	_ = repo.Upsert(context.Background(), "7203", []market.Candle{
		dailyBarAt("7203", 1000, now.AddDate(0, 0, -12)),
		dailyBarAt("7203", 1010, now.AddDate(0, 0, -11)), // newest is 11 days old → stale
	})
	b := &SymbolBundle{Symbol: "7203", Candles: repo, Clock: clock.Fixed(now), Counters: &Counters{}}

	if got := b.dailyCandles(context.Background()); got != nil {
		t.Fatalf("a stale daily feed must return nil, got %d bars", len(got))
	}
	if b.Counters.StaleDaily.Load() != 1 {
		t.Fatalf("StaleDaily counter = %d, want 1", b.Counters.StaleDaily.Load())
	}
}

// 🚨 鮮度は**営業日**で数える。暦日 5 日だと、シルバーウィーク
// (9/19 土〜9/23 水)明けの 9/24(木)は最新バー 9/18(金)から 6 日で「凍結」と読み、
// 3 トラック全部の日足戦略が連休明けの 1 日を丸ごと沈黙する(年始 1/4 も同じ)。
func TestDailyCandles_HolidayRunIsNotStale(t *testing.T) {
	hours := session.TradingHours{TZ: clock.JST, Holidays: map[string]struct{}{
		"2026-09-21": {}, "2026-09-22": {}, "2026-09-23": {},
	}}
	now := time.Date(2026, 9, 24, 9, 0, 0, 0, clock.JST) // 連休明けの寄り
	repo := repository.NewInMemoryCandleRepo()
	_ = repo.Upsert(context.Background(), "7203", []market.Candle{
		dailyBarAt("7203", 1000, time.Date(2026, 9, 17, 0, 0, 0, 0, clock.JST)),
		dailyBarAt("7203", 1010, time.Date(2026, 9, 18, 0, 0, 0, 0, clock.JST)), // 連休前の最終営業日
	})
	b := &SymbolBundle{Symbol: "7203", Candles: repo, Hours: hours, Clock: clock.Fixed(now), Counters: &Counters{}}

	if got := b.dailyCandles(context.Background()); len(got) != 2 {
		t.Fatalf("連休明けの日足を凍結と読んだ(%d bars)— 営業日で数えれば 1 日しか経っていない", len(got))
	}
	if b.Counters.StaleDaily.Load() != 0 {
		t.Fatal("fresh feed must not bump StaleDaily")
	}
}

// 営業日で 4 日取りこぼしたら凍結(暦日 5 日と同じ許容 = 金曜のバーを水曜まで許す)。
func TestDailyCandles_FourMissedTradingDaysIsStale(t *testing.T) {
	hours := session.TradingHours{TZ: clock.JST}
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, clock.JST) // 金
	repo := repository.NewInMemoryCandleRepo()
	_ = repo.Upsert(context.Background(), "7203", []market.Candle{
		dailyBarAt("7203", 1010, time.Date(2026, 9, 14, 0, 0, 0, 0, clock.JST)), // 月 → 火水木金の 4 営業日分が無い
	})
	b := &SymbolBundle{Symbol: "7203", Candles: repo, Hours: hours, Clock: clock.Fixed(now), Counters: &Counters{}}

	if got := b.dailyCandles(context.Background()); got != nil {
		t.Fatalf("4 営業日古い日足を通した(%d bars)", len(got))
	}
	if b.Counters.StaleDaily.Load() != 1 {
		t.Fatalf("StaleDaily counter = %d, want 1", b.Counters.StaleDaily.Load())
	}
}

func TestDailyCandles_FreshFeedPasses(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	repo := repository.NewInMemoryCandleRepo()
	_ = repo.Upsert(context.Background(), "7203", []market.Candle{
		dailyBarAt("7203", 1000, now.AddDate(0, 0, -2)),
		dailyBarAt("7203", 1010, now.AddDate(0, 0, -1)), // yesterday → fresh
	})
	b := &SymbolBundle{Symbol: "7203", Candles: repo, Clock: clock.Fixed(now), Counters: &Counters{}}

	if got := b.dailyCandles(context.Background()); len(got) != 2 {
		t.Fatalf("a fresh daily feed must pass through, got %d bars", len(got))
	}
	if b.Counters.StaleDaily.Load() != 0 {
		t.Fatal("fresh feed must not bump StaleDaily")
	}
}

// 🛑 判断が付かないときは **true**(= 照会する)。建玉を持っているかもしれない口座で
// 「無保有」に倒すと、reconcile の間引きが守りの間引きに化ける。
func TestHoldsPosition(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()

	// PosRepo 未配線 = 判断不能 → true
	if !(&SymbolBundle{Symbol: "7203"}).HoldsPosition(ctx) {
		t.Error("PosRepo なしで false(判断不能を無保有に倒している)")
	}
	b := &SymbolBundle{Symbol: "7203", PosRepo: repo}
	if b.HoldsPosition(ctx) {
		t.Error("建玉ゼロなのに true")
	}
	if _, err := repo.AdoptExternal(ctx, port.BrokerPosition{
		BrokerPositionID: "bp-1", Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		EntryPrice: 2500, ExecKind: order.ExecCash,
	}, time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !b.HoldsPosition(ctx) {
		t.Error("建玉があるのに false")
	}
	// 別銘柄の建玉は自分の照会理由にならない。
	if (&SymbolBundle{Symbol: "6758", PosRepo: repo}).HoldsPosition(ctx) {
		t.Error("別銘柄の建玉で true")
	}
}

// 🛑 **repo から読んだ日足にも分割の段差ガードを掛ける**。
//
// broker 直読み経路(rawDailyCandles の後半)には既に `SplitDiscontinuity` が
// 掛かっていたのに、**repo 経路(通常はこちら)には無かった**。結果、DB に未調整の
// 分割が残っていると:
//   - 25日線が実勢の 3〜4 倍になり、乖離が -65〜-84% の偽のパニックになる
//   - 逆張り(BNF)はそれを最良のエントリー候補として選ぶ = **構造的に引き寄せられる**
//   - ATR も膨らみ SL が建値の 50% に化ける
//
// live トラックが分割未調整の銘柄を arm したのがこれ。fat-finger backstop が
// 偶然止めただけで、守りの設計ではなかった。
//
// **取り込みが正しいことを願う**のではなく、**壊れたデータを使わせない**。
func TestDailyCandles_RejectsSplitSeamFromRepo(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryCandleRepo()
	base := time.Date(2026, 7, 27, 0, 0, 0, 0, clock.JST)
	// 1:4 分割の段差が残った系列(6842 → 1672)。
	bars := []market.Candle{
		{OpenTime: base, Close: 6881, High: 6900, Low: 6800, Open: 6850, Interval: 24 * time.Hour},
		{OpenTime: base.AddDate(0, 0, 2), Close: 6842, High: 6900, Low: 6800, Open: 6850, Interval: 24 * time.Hour},
		{OpenTime: base.AddDate(0, 0, 3), Close: 1672, High: 1700, Low: 1650, Open: 1690, Interval: 24 * time.Hour},
		{OpenTime: base.AddDate(0, 0, 4), Close: 1654, High: 1680, Low: 1640, Open: 1670, Interval: 24 * time.Hour},
	}
	if err := repo.Upsert(ctx, "8309", bars); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	b := &SymbolBundle{
		Symbol: "8309", Candles: repo, Counters: &Counters{},
		Clock:  clock.Fixed(base.AddDate(0, 0, 4)),
		Logger: testutil.SilentLogger(),
	}
	if got := b.dailyCandles(ctx); got != nil {
		t.Fatalf("分割の段差が残った日足を戦略に渡した(%d本)— 偽のパニックで逆張りが発火する", len(got))
	}
}

// 段差の無い正常な系列は当然通す(ガードが全部を殺さないこと)。
func TestDailyCandles_PassesCleanSeries(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryCandleRepo()
	base := time.Date(2026, 7, 27, 0, 0, 0, 0, clock.JST)
	var bars []market.Candle
	for i := 0; i < 5; i++ {
		bars = append(bars, market.Candle{
			OpenTime: base.AddDate(0, 0, i), Open: 1700, High: 1720, Low: 1680,
			Close: 1700 + float64(i), Interval: 24 * time.Hour,
		})
	}
	if err := repo.Upsert(ctx, "7203", bars); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	b := &SymbolBundle{
		Symbol: "7203", Candles: repo, Counters: &Counters{},
		Clock:  clock.Fixed(base.AddDate(0, 0, 4)),
		Logger: testutil.SilentLogger(),
	}
	if got := b.dailyCandles(ctx); len(got) != 5 {
		t.Fatalf("正常な系列が %d 本になった, want 5", len(got))
	}
}

// reconcile の照会条件に使う。「台帳に建玉がある銘柄」だけを見ると、台帳に無い
// 建玉は構造的に発見できない。arm 済みの銘柄も照会対象に含める。
func TestSymbolBundle_IsArmed(t *testing.T) {
	b := &SymbolBundle{Symbol: "7203", Configs: NewConfigSet(
		&config.StrategyConfig{ConfigID: "no_trade_7203", Symbol: "7203", StrategyName: config.StrategyNoTrade})}
	if b.IsArmed() {
		t.Fatal("no_trade は arm 済みではない")
	}
	b.Configs.Arm(&config.StrategyConfig{ConfigID: "bnf_7203", Symbol: "7203", StrategyName: config.StrategyBNFReversion})
	if !b.IsArmed() {
		t.Fatal("戦略が入っているのに arm 済みでない")
	}
	if (&SymbolBundle{Symbol: "7203"}).IsArmed() {
		t.Fatal("Config が nil なら arm 済みではない")
	}
}
