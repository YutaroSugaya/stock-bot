package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"time"

	"stockbot/backend/internal/adapter/candlecsv"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// refreshDailyCandles re-fetches each symbol's recent daily bars and upserts
// them. It NEVER wipes: an empty/errored fetch keeps the last-known bars, and the
// bundle's freshness guard blocks trading if the feed stays stale.
//
// Symbols whose stored history already reaches freshThrough are SKIPPED without
// an API call — restarts happen at arbitrary times and re-pulling 222 銘柄 every
// time was pure waste (立花 から高負荷と指摘された経路)。Empty freshThrough = unknown
// → fetch everything, because serving a day-horizon strategy stale bars is worse
// than one extra call.
func refreshDailyCandles(ctx context.Context, repo port.CandleRepository, broker port.Broker, symbols []string, freshThrough string, logger *slog.Logger) int {
	if repo == nil || broker == nil {
		return 0
	}
	behind := make([]string, 0, len(symbols))
	skipped := 0
	for _, sym := range symbols {
		if storedDailyReaches(ctx, repo, sym, freshThrough) {
			skipped++
			continue
		}
		behind = append(behind, sym)
	}
	if skipped > 0 {
		logger.Info("daily refresh: skipped symbols already current", "skipped", skipped, "through", freshThrough)
	}
	if len(behind) == 0 {
		return 0
	}
	// カナリア: 立花は当日の日足を引け後すぐには配信しない(実測: 18:50
	// 時点でも最新は前営業日)。全銘柄を引いて初めて「まだ無い」と分かる作りだと、
	// 推奨窓の毎時ティックで 222銘柄 × 9回 ≈ 2,000 リクエストの空振りになる。
	probed := map[string][]market.Candle{}
	if freshThrough != "" {
		published, fetched := probeDailyCanaries(ctx, broker, behind, freshThrough, logger)
		if !published {
			logger.Info("daily refresh: broker has no bar for the target date yet — round aborted",
				"through", freshThrough, "probes", len(fetched), "pending", len(behind))
			return 0
		}
		probed = fetched // カナリアで取った分は再利用する(二重取得しない)
	}
	n := 0
	for _, sym := range behind {
		cs, ok := probed[sym]
		var err error
		if !ok {
			cs, err = broker.GetKlines(ctx, sym, port.PeriodDaily, 250)
		}
		if err != nil {
			logger.Warn("daily refresh: klines fetch failed (keeping last-known bars)", "symbol", sym, "err", err)
			continue
		}
		if len(cs) == 0 {
			continue // broker served no klines (e.g. paper)
		}
		// 立花の日足は **分割未調整**: 1:5 分割が見かけ -80% の
		// バーとして入り、day-horizon 戦略の乖離判定を毒する。断裂するリフレッシュは
		// 捨てる — 古い値を残す方が安全(bundle の stale ガードが取引を止める)。
		if bad := discontinuityVs(ctx, repo, sym, cs); bad != "" {
			logger.Warn("daily refresh: split-sized discontinuity — dropping fetched bars; run `make fetch-daily` to chain-link the split",
				"symbol", sym, "detail", bad)
			continue
		}
		if err := repo.Upsert(ctx, sym, cs); err != nil {
			logger.Warn("daily refresh: upsert failed", "symbol", sym, "err", err)
			continue
		}
		n++
	}
	return n
}

// maxDailyCanaryProbes: more than one so a single halted or delisted issue
// cannot stall the whole universe's refresh forever.
const maxDailyCanaryProbes = 3

// probeDailyCanaries asks a few symbols whether the broker already publishes a
// bar on/after `through`, returning that verdict plus the bars it fetched.
//
// An error or an empty answer counts as "unknown → proceed": saving calls must
// never turn into silently never refreshing.
func probeDailyCanaries(ctx context.Context, broker port.Broker, candidates []string, through string, logger *slog.Logger) (bool, map[string][]market.Candle) {
	fetched := map[string][]market.Candle{}
	for i, sym := range candidates {
		if i >= maxDailyCanaryProbes {
			return false, fetched
		}
		cs, err := broker.GetKlines(ctx, sym, port.PeriodDaily, 250)
		if err != nil {
			logger.Warn("daily refresh: canary probe failed — proceeding", "symbol", sym, "err", err)
			return true, fetched // 判断できないなら従来どおり回す
		}
		if len(cs) == 0 {
			return true, fetched // klines を出さない broker(paper 等)
		}
		fetched[sym] = cs
		newest := cs[0].OpenTime
		for _, c := range cs {
			if c.OpenTime.After(newest) {
				newest = c.OpenTime
			}
		}
		if newest.In(clock.JST).Format("2006-01-02") >= through {
			return true, fetched
		}
	}
	return false, fetched
}

// storedDailyReaches reports whether the stored daily history already includes a
// bar on/after through. A read failure or an unknown `through` answers false =
// fetch (never skip on doubt).
func storedDailyReaches(ctx context.Context, repo port.CandleRepository, symbol, through string) bool {
	if through == "" {
		return false
	}
	// List makes no ordering promise, so scan for the max instead of taking [0].
	cs, err := repo.List(ctx, symbol, port.PeriodDaily, 5)
	if err != nil || len(cs) == 0 {
		return false
	}
	newest := cs[0].OpenTime
	for _, c := range cs {
		if c.OpenTime.After(newest) {
			newest = c.OpenTime
		}
	}
	// Compare in the venue TZ: a pg TIMESTAMPTZ read back as UTC would otherwise
	// shift the JST bucket a day.
	return newest.In(clock.JST).Format("2006-01-02") >= through
}

// discontinuityVs reports (non-empty) when the fetched bars would poison the
// stored history(判定は market.SeamBreak。既存履歴の内部は見ない)。
func discontinuityVs(ctx context.Context, repo port.CandleRepository, symbol string, fetched []market.Candle) string {
	stored, err := repo.List(ctx, symbol, port.PeriodDaily, 400)
	if err != nil {
		// fail-close: 履歴と突き合わせられないなら取り込まない。
		return fmt.Sprintf("既存履歴を読めないため検証不能: %v", err)
	}
	return market.SeamBreak(stored, fetched)
}

// seedDailyCandles loads each symbol's daily CSV into the candle repo so
// day-horizon strategies have history without a live klines source. A symbol with
// no file is skipped (not an error) so a partial universe still seeds what it has.
func seedDailyCandles(ctx context.Context, repo port.CandleRepository, symbols []string, dir string) (int, error) {
	if repo == nil {
		return 0, nil
	}
	total := 0
	for _, sym := range symbols {
		candles, err := loadSeedCSV(dir, sym)
		if err != nil {
			return total, fmt.Errorf("seed %s: %w", sym, err)
		}
		if len(candles) == 0 {
			continue
		}
		if err := repo.Upsert(ctx, sym, candles); err != nil {
			return total, fmt.Errorf("upsert %s: %w", sym, err)
		}
		total += len(candles)
	}
	return total, nil
}

// loadSeedCSV tries <dir>/<sym>.csv then <dir>/<sym>_daily.csv (the cmd/backtest
// naming), so one data set drives backtests, scan and the bot. A missing file
// yields (nil, nil).
func loadSeedCSV(dir, sym string) ([]market.Candle, error) {
	for _, name := range []string{sym + ".csv", sym + "_daily.csv"} {
		cs, err := candlecsv.Load(filepath.Join(dir, name), sym, 24*time.Hour)
		if err == nil {
			return cs, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return nil, nil
}
