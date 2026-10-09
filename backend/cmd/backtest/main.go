// Command backtest replays a candle CSV through the production strategy + risk
// gate + exit logic and prints a net-of-cost edge report. It is
// READ-ONLY: it never writes trades/positions and never touches a live DB.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"stockbot/backend/internal/adapter/candlecsv"
	"stockbot/backend/internal/backtest"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
)

func main() {
	var (
		csvPath      = flag.String("csv", "", "candle CSV path (TimestampJST;O;H;L;C;V)")
		symbol       = flag.String("symbol", "7203", "symbol code")
		interval     = flag.String("interval", "1d", "bar interval: 1m|5m|1h|1d")
		stratName    = flag.String("strategy", "time_series_momentum", "strategy name")
		tpTicks      = flag.Float64("tp", 100, "take-profit ticks")
		slTicks      = flag.Float64("sl", 50, "stop-loss ticks")
		maxHold      = flag.Int("max-hold", 0, "max hold minutes (0=none)")
		ratchetArm   = flag.Float64("ratchet-arm", 0, "ratchet arm ticks (0=off; >0 = trailing 'let winners run')")
		ratchetGive  = flag.Float64("ratchet-giveback", 0, "ratchet giveback ticks (exit on this much peak giveback once armed)")
		qty          = flag.Int("qty", 100, "quantity")
		holding      = flag.String("holding", "multiday", "intraday|multiday")
		feeRate      = flag.Float64("fee-rate", backtest.DefaultFeeRatePct, "one-side commission %")
		slippage     = flag.Float64("slippage-ticks", backtest.DefaultSlippageTicks, "adverse ticks per leg")
		spreadTicks  = flag.Float64("spread-ticks", 1, "constant spread in ticks")
		conflictMode = flag.String("conflict", "pessimistic", "pessimistic|optimistic|skip (TP&SL same bar)")
		maxDailyLoss = flag.Int("max-daily-loss-jpy", 0, "daily loss cap (0=disabled)")
		maxConsec    = flag.Int("max-consecutive-losses", 0, "consecutive-loss cap (0=disabled)")
		bnfDev       = flag.Float64("bnf-dev", 0, "BNF override: 25MA deviation entry threshold (e.g. -0.12; 0=default)")
		bnfVol       = flag.Float64("bnf-vol", 0, "BNF override: volume-ratio entry threshold (e.g. 1.5; 0=default)")
		bnfStop      = flag.Float64("bnf-stop-atr", 0, "BNF override: stop-loss を ATR の何倍にするか (e.g. 2.0; 0=default)")
		dailyCSV     = flag.String("daily-csv", "", "daily-bar CSV injected as context into a sub-daily replay (dual-timeframe strategies, e.g. bnf_intraday_reversion)")
		jsonOut      = flag.Bool("json", false, "emit full Result JSON (default: summary)")
		hardLimits   = flag.String("hard-limits", "../configs/hard_limits.yaml", "信用の料率・受渡日数・休場カレンダー(carry を本番と同じモデルで出す)")
		botConfig    = flag.String("bot-config", "../configs/bot_config.advisor.yaml", "保有区分ごとの執行区分(paper と同じ ExecKindFor。信用の多日建玉にだけ carry が付く)")
	)
	flag.Parse()

	if *csvPath == "" {
		fmt.Fprintln(os.Stderr, "usage: backtest -csv path [flags]")
		os.Exit(2)
	}
	iv, err := parseInterval(*interval)
	if err != nil {
		fatal(err)
	}
	candles, err := candlecsv.Load(*csvPath, *symbol, iv)
	if err != nil {
		fatal(err)
	}

	cfg := &config.StrategyConfig{
		ConfigID: "backtest", Symbol: *symbol, StrategyName: config.StrategyName(*stratName),
		HoldingMode: config.HoldingMode(*holding),
	}
	cfg.Entry.Direction = config.DirectionBoth
	cfg.Entry.MaxSpreadTicks = 0 // do not spread-gate in backtest; cost model handles cost
	cfg.Exit.TakeProfitJPY = *tpTicks
	cfg.Exit.StopLossJPY = *slTicks
	cfg.Exit.MaxHoldMinutes = *maxHold
	cfg.Exit.RatchetArmJPY = *ratchetArm
	cfg.Exit.RatchetGivebackJPY = *ratchetGive
	cfg.Risk.Quantity = *qty
	cfg.Risk.MaxOpenPositions = 1
	if *bnfDev != 0 || *bnfVol != 0 || *bnfStop != 0 {
		cfg.Tuning = map[string]float64{}
		if *bnfDev != 0 {
			cfg.Tuning["bnf_dev"] = *bnfDev
		}
		if *bnfVol != 0 {
			cfg.Tuning["bnf_vol"] = *bnfVol
		}
		if *bnfStop != 0 {
			cfg.Tuning["bnf_stop_atr"] = *bnfStop
		}
	}

	strat := strategy.ByName(config.StrategyName(*stratName)) // 戦略カタログ(メニュー外の再現用も引ける)
	if strat == nil {
		fatal(fmt.Errorf("unknown strategy %q", *stratName))
	}

	// carry は本番と同じモデル(D-12)。読めなければ落とす — 黙って carry 0 で回すと net が良く出る。
	hl, err := config.LoadHardLimits(*hardLimits)
	if err != nil {
		fatal(fmt.Errorf("-hard-limits: %w", err))
	}
	venue, err := hl.SessionHours.TradingHours()
	if err != nil {
		fatal(fmt.Errorf("-hard-limits の session_hours: %w", err))
	}
	botCfg, err := config.LoadBotConfig(*botConfig)
	if err != nil {
		fatal(fmt.Errorf("-bot-config: %w", err))
	}

	cost := backtest.CostModel{
		SlippageTicks: *slippage, FeeRatePct: *feeRate, Spread: backtest.ConstSpread(*spreadTicks),
		Carry: hl.CarryCalc(venue),
	}
	eng := backtest.NewEngine(cfg, strat, cost, conflictPolicy(*conflictMode))
	eng.ExecKindFor = botCfg.ExecKindFor
	eng.MaxDailyLossJPY = *maxDailyLoss
	eng.MaxConsecutiveLosses = *maxConsec
	if *dailyCSV != "" {
		if iv >= 24*time.Hour {
			fatal(fmt.Errorf("-daily-csv only applies to a sub-daily replay (interval %s)", *interval))
		}
		daily, err := candlecsv.Load(*dailyCSV, *symbol, 24*time.Hour)
		if err != nil {
			fatal(err)
		}
		eng.DailyContext = daily
	}
	// Session gating only applies to intraday replay; daily bars have no
	// intraday session window.
	if iv < 24*time.Hour {
		eng.Hours = tokyoHours()
	}
	res, err := eng.Replay(context.Background(), candles)
	if err != nil {
		fatal(err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if *jsonOut {
		_ = enc.Encode(res)
		return
	}
	_ = enc.Encode(map[string]any{
		"symbol": res.Symbol, "candles": len(candles), "ambiguous_bars": res.AmbiguousBars, "metrics": res.Metrics,
	})
}

func parseInterval(s string) (time.Duration, error) {
	switch s {
	case "1m":
		return time.Minute, nil
	case "5m":
		return 5 * time.Minute, nil
	case "1h":
		return time.Hour, nil
	case "1d":
		return 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("bad interval %q (use 1m|5m|1h|1d)", s)
	}
}

func tokyoHours() session.TradingHours {
	return session.TradingHours{
		TZ:          clock.JST,
		Sessions:    []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:00"}},
		EntryCutoff: "14:55",
		ForceFlatAt: "14:50",
	}
}

func conflictPolicy(s string) backtest.ConflictPolicy {
	switch s {
	case "optimistic":
		return backtest.OptimisticTPFirst
	case "skip":
		return backtest.SkipAmbiguous
	default:
		return backtest.PessimisticSLFirst
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "backtest:", err)
	os.Exit(1)
}
