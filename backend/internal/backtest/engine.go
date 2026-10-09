package backtest

import (
	"context"
	"fmt"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
)

// ConflictPolicy resolves a bar that touches both TP and SL: the intrabar path
// is unknown, so the choice must be explicit and fixed to keep replays reproducible.
type ConflictPolicy int

const (
	PessimisticSLFirst ConflictPolicy = iota
	OptimisticTPFirst
	SkipAmbiguous
)

// Engine replays one symbol's candles through the production strategy + risk
// gate + exit logic. Determinism: bar-close clock, counter SignalID, no rand.
type Engine struct {
	Symbol         string
	Config         *config.StrategyConfig
	Strat          strategy.Strategy
	Cost           CostModel
	Conflict       ConflictPolicy
	Hours          session.TradingHours
	MaxHistoryBars int
	Seed           int64
	// ExecKindFor は建玉の執行区分(本番の bot_config.ExecKindFor と同じ)。建玉時に Position へ
	// 凍結し、carry はそれで決まる。nil = 執行区分なし = carry 0(料率を捏造しない)。
	ExecKindFor func(order.HoldingMode) order.ExecKind

	// Fed into the replay snapshot so the same risk gate that brakes live also
	// fires here — a backtest without these caps overstates the edge. 0 disables.
	MaxDailyLossJPY         int
	MaxConsecutiveLosses    int
	AccountMaxDailyLossJPY  int
	AccountMaxOpenPositions int

	// DailyContext gives a SUB-DAILY replay its daily-bar history. Only bars
	// strictly before the current bar's JST session are passed: today's daily
	// close is not knowable intraday. Ignored when replaying daily candles.
	DailyContext []market.Candle

	ambiguous int // not concurrency-safe; scoped to one Replay call
}

func NewEngine(cfg *config.StrategyConfig, strat strategy.Strategy, cost CostModel, conflict ConflictPolicy) *Engine {
	return &Engine{
		Symbol: cfg.Symbol, Config: cfg, Strat: strat, Cost: cost, Conflict: conflict,
		MaxHistoryBars: 20000,
	}
}

type openPos struct {
	pos       position.Position
	openedBar int
	rawEntry  float64 // un-slippaged fill reference; keeps GROSS cost-model-independent
}

func (e *Engine) Replay(_ context.Context, candles []market.Candle) (Result, error) {
	if e.Config == nil || e.Strat == nil {
		return Result{}, fmt.Errorf("backtest: engine requires Config and Strat")
	}
	seq := 0
	eng := strategy.NewEngine(func() string { seq++; return fmt.Sprintf("bt-%d", seq) }, e.Strat)

	var open []*openPos
	var trades []Trade
	res := Result{Symbol: e.Symbol}

	consecLoss := 0
	dayKey := ""
	dayLossJPY := 0

	for i := range candles {
		bar := candles[i]
		now := bar.OpenTime.Add(bar.Interval)

		if k := now.Format("2006-01-02"); k != dayKey {
			dayKey = k
			dayLossJPY = 0
		}

		// A position is eligible for exit from its FILL bar onward: an entry decided
		// on bar i fills at bar i+1's open, so the fill bar can already stop out, but
		// a bar strictly after the current one never can (no look-ahead).
		stillOpen := open[:0]
		for _, op := range open {
			if op.openedBar > i {
				stillOpen = append(stillOpen, op)
				continue
			}
			reason, touch, exited := e.resolveExit(op, bar, now)
			if !exited {
				stillOpen = append(stillOpen, op)
				continue
			}
			tr := e.closeTrade(op, touch, reason, now)
			trades = append(trades, tr)
			if tr.NetJPY <= 0 {
				consecLoss++
				dayLossJPY += int(-tr.NetJPY)
			} else {
				consecLoss = 0
			}
		}
		open = stillOpen

		summary := e.summary(bar, now)
		in := strategy.EvalInput{
			Now:          now,
			Summary:      summary,
			Config:       e.Config,
			CandlesDaily: nil,
			// 戦略別 MaxHold の営業日換算に使う。日足リプレイでは Hours が
			// zero 値なので週末だけを飛ばす換算に縮退する(祝日は数えない)。
			// 黙って「無期限」にはしない — それでは戦略別 MaxHold が backtest から消える。
			Hours: e.Hours,
		}
		e.fillWindows(&in, candles, i)
		sig := eng.Evaluate(in)
		// Decided on bar i's CLOSE, so the order can only FILL at bar i+1's OPEN;
		// filling at bar i's own close is look-ahead the live path cannot achieve.
		// No next bar → the entry cannot fill and is dropped.
		if sig.IsEntry() && i+1 < len(candles) {
			snap := e.snapshot(now, open, consecLoss, dayLossJPY, sig)
			if d := risk.EvaluateSignal(sig, e.Config, snap, summary); d.Allowed {
				qty := applyQty(sig.Quantity, d.QtyMultiplier)
				next := candles[i+1]
				fillRef := next.Open
				fillNow := next.OpenTime
				entryFill := e.Cost.EntryFill(e.Symbol, sig.Side, fillRef, fillNow)
				pos := positionFromSignal(sig, entryFill, qty, fillNow)
				if e.ExecKindFor != nil {
					pos.ExecKind = e.ExecKindFor(pos.HoldingMode) // 建玉時に凍結(本番と同じ)
				}
				open = append(open, &openPos{pos: pos, openedBar: i + 1, rawEntry: fillRef})
			}
		}
	}

	if len(candles) > 0 && len(open) > 0 {
		last := candles[len(candles)-1]
		now := last.OpenTime.Add(last.Interval)
		for _, op := range open {
			trades = append(trades, e.closeTrade(op, last.Close, "end_of_backtest", now))
		}
	}

	res.Trades = trades
	res.Metrics = computeMetrics(trades)
	res.Equity = equityCurve(trades)
	res.AmbiguousBars = e.ambiguous
	return res, nil
}

func (e *Engine) resolveExit(op *openPos, bar market.Candle, now time.Time) (reason string, touch float64, exited bool) {
	p := op.pos
	// Trigger levels come from the RAW entry, not the slippaged fill, so the trade
	// set and GROSS stay cost-model-independent and net-vs-gross isolates the full
	// cost floor. Net still uses the fills.
	tp, sl := position.TPSLPricesFromJPY(p.Symbol, p.Side, op.rawEntry, p.TakeProfitJPY, p.StopLossJPY)

	var hitTP, hitSL bool
	if p.Side == order.SideBuy {
		hitTP = tp > 0 && bar.High >= tp
		hitSL = sl > 0 && bar.Low <= sl
	} else {
		hitTP = tp > 0 && bar.Low <= tp
		hitSL = sl > 0 && bar.High >= sl
	}

	if hitTP && hitSL {
		e.ambiguous++
		switch e.Conflict {
		case OptimisticTPFirst:
			return "take_profit", tp, true
		case SkipAmbiguous:
			return "", 0, false
		default: // PessimisticSLFirst
			return "stop_loss", stopFill(p.Side, sl, bar.Open), true
		}
	}
	if hitTP {
		return "take_profit", tp, true
	}
	if hitSL {
		return "stop_loss", stopFill(p.Side, sl, bar.Open), true
	}

	// The favourable intrabar extreme must reach the ratchet peak BEFORE the
	// close-based exit check, else a peak hit intrabar and given back by the close
	// never triggers, unlike live. The adverse extreme (MAE) rides along: no exit
	// rule reads it, it is the record a counterfactual trail is measured from.
	favExtreme, advExtreme := bar.High, bar.Low
	if p.Side == order.SideSell {
		favExtreme, advExtreme = bar.Low, bar.High
	}
	if fav := op.pos.UnrealizedJPY(favExtreme); fav > op.pos.PeakUnrealizedJPY {
		op.pos.PeakUnrealizedJPY = fav
	}
	if adv := op.pos.UnrealizedJPY(advExtreme); adv < op.pos.TroughUnrealizedJPY {
		op.pos.TroughUnrealizedJPY = adv
	}

	d := position.EvaluateExit(op.pos, bar.Close, now)
	if d.Exit {
		return d.Reason, bar.Close, true
	}
	if d.ExcursionChanged {
		op.pos.PeakUnrealizedJPY = d.NewPeak
		op.pos.TroughUnrealizedJPY = d.NewTrough
		op.pos.RatchetArmed = d.NewArmed
	}
	return "", 0, false
}

func (e *Engine) closeTrade(op *openPos, touch float64, reason string, now time.Time) Trade {
	p := op.pos
	exitFill := e.Cost.ExitFill(e.Symbol, p.Side, touch, now)
	// GROSS uses raw prices and NET the slippaged fills, so net - gross equals the
	// full cost floor.
	gross := signedGross(p.Side, op.rawEntry, touch, p.Quantity)
	fillGross := signedGross(p.Side, p.EntryPrice, exitFill, p.Quantity)
	fee := e.Cost.FeeJPY(p.EntryPrice, exitFill, p.Quantity)
	carry := e.Cost.CarryJPY(p, now)
	return Trade{
		Symbol: p.Symbol, Side: p.Side, EntryPrice: p.EntryPrice, ExitPrice: exitFill, Quantity: p.Quantity,
		OpenedAt: p.OpenedAt, ClosedAt: now, GrossJPY: gross, FeeJPY: fee, CarryJPY: carry,
		NetJPY: fillGross - fee + carry, CloseReason: reason,
	}
}

// summary synthesises a MarketSummary whose spread matches the cost model.
func (e *Engine) summary(bar market.Candle, now time.Time) *market.MarketSummary {
	half := 0.0
	if e.Cost.Spread != nil {
		half = e.Cost.Spread.SpreadTicks(now) / 2.0 * market.TickSizeOf(e.Symbol, bar.Close)
	}
	t := market.Ticker{Symbol: e.Symbol, Bid: bar.Close - half, Ask: bar.Close + half, Last: bar.Close, Timestamp: now}
	return market.SummaryFromTicker(t, now)
}

// fillWindows slices history into the window matching the bar interval. A
// sub-daily replay also gets DailyContext cut to bars strictly before the current
// bar's JST session: yesterday's close is knowable intraday, today's is not.
func (e *Engine) fillWindows(in *strategy.EvalInput, candles []market.Candle, i int) {
	start := 0
	if e.MaxHistoryBars > 0 && i+1-e.MaxHistoryBars > 0 {
		start = i + 1 - e.MaxHistoryBars
	}
	hist := candles[start : i+1]
	switch iv := candles[i].Interval; {
	case iv >= 24*time.Hour:
		in.CandlesDaily = hist
	case iv >= time.Hour:
		in.Candles1h = hist
	case iv >= 5*time.Minute:
		in.Candles5m = hist
	default:
		in.Candles1m = hist
	}
	if candles[i].Interval < 24*time.Hour && len(e.DailyContext) > 0 {
		in.CandlesDaily = strategy.ConfirmedDailyBefore(e.DailyContext, candles[i].OpenTime)
	}
}

// 🛑 sig を受け取るのは **(銘柄, 戦略) キーのカウンタ**を埋めるため。
// 埋め忘れると paper 判定の建玉枠とナンピン禁止が backtest で **fail-open** になり、
// live では建たない積み増しが backtest でだけ通る = 検定が実行と別物になる
// (backtest がナンピン禁止を検証していない形になっていた)。
func (e *Engine) snapshot(now time.Time, open []*openPos, consecLoss, dayLossJPY int, sig strategy.Signal) risk.AccountSnapshot {
	snap := risk.AccountSnapshot{
		Now:                     now,
		ConsecutiveLosses:       consecLoss,
		DailyLossJPY:            dayLossJPY,
		AccountDailyLossJPY:     dayLossJPY,
		MaxDailyLossJPY:         e.MaxDailyLossJPY,
		MaxConsecutiveLosses:    e.MaxConsecutiveLosses,
		AccountMaxDailyLossJPY:  e.AccountMaxDailyLossJPY,
		AccountMaxOpenPositions: e.AccountMaxOpenPositions,
		OutsideSessionHours:     e.Hours.TZ != nil && !e.Hours.InTradingHours(now),
		EndOfDayCloseRequired:   e.Hours.TZ != nil && e.Hours.IsAfterEntryCutoff(now),
	}
	for _, op := range open {
		// backtest は 1 銘柄 1 戦略のリプレイなので実際には常に同一戦略だが、
		// **カウンタを埋めること自体**が契約(埋めないと paper 判定が素通りする)。
		// 戦略不明("")は本番と同じく全戦略に数える fail-close。
		same := op.pos.StrategyName == "" || op.pos.StrategyName == string(sig.StrategyName)
		snap.OpenPositions++
		if same {
			snap.OpenPositionsSameStrategy++
		}
		switch op.pos.Side {
		case order.SideBuy:
			snap.OpenBuyInclExternal++
			if same {
				snap.OpenBuySameStrategyInclExternal++
			}
		case order.SideSell:
			snap.OpenSellInclExternal++
			if same {
				snap.OpenSellSameStrategyInclExternal++
			}
		}
	}
	return snap
}

func positionFromSignal(sig strategy.Signal, entryFill float64, qty int, now time.Time) position.Position {
	return position.Position{
		Symbol: sig.Symbol, Side: sig.Side, Quantity: qty, EntryPrice: entryFill,
		TakeProfitJPY: sig.TakeProfitJPY, StopLossJPY: sig.StopLossJPY, MaxHoldMinutes: sig.MaxHoldMinutes,
		ExtensionMaxMinutes: sig.ExtensionMaxMinutes, ExtensionUnrealizedJPY: sig.ExtensionUnrealizedJPY,
		EarlyExitWindowMinutes: sig.EarlyExitWindowMinutes, EarlyExitTargetJPY: sig.EarlyExitTargetJPY,
		RatchetArmJPY: sig.RatchetArmJPY, RatchetGivebackJPY: sig.RatchetGivebackJPY,
		// backtest は**現行の規則**を測る。live の entry saga と同じく床あり。
		// ここを落とすと backtest と forward が別の出口を測ることになる。
		RatchetFloorAtArm: true,
		StrategyConfigID:  sig.ConfigID, StrategyName: string(sig.StrategyName),
		HoldingMode: sig.HoldingMode, TickSizeAtEntry: market.TickSizeOf(sig.Symbol, entryFill),
		Source: position.SourceBot, Status: position.StatusOpen, OpenedAt: now,
	}
}

// stopFill fills at the worse OPEN when the bar gapped through the stop: you
// cannot get filled at a level the price jumped over, and filling at the stop
// level understates gap/crash losses.
func stopFill(side order.Side, sl, open float64) float64 {
	if side == order.SideBuy {
		if open < sl {
			return open
		}
		return sl
	}
	if open > sl {
		return open
	}
	return sl
}

func signedGross(side order.Side, entry, exit float64, qty int) float64 {
	diff := exit - entry
	if side == order.SideSell {
		diff = -diff
	}
	return diff * float64(qty)
}

func applyQty(qty int, mul float64) int {
	if mul <= 0 {
		mul = 1.0
	}
	q := int(float64(qty) * mul)
	if q < 1 {
		return 1
	}
	return q
}
