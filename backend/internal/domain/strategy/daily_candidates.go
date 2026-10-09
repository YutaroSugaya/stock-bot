package strategy

import (
	"math"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/ta"
)

// どれも証明済みエッジではなく候補テンプレ。定数は検定の**前**に置いた事前登録値で、事後チューニング禁止。
// no-look-ahead 契約: 入口は現バーの終値で決め、harness はその終値で入り出口は厳密に後のバーでしか見ない。
// ブレイクアウト / 52週高値は live バーを除いた確定足に対して判定する(現バーの高値を覗くと自己成就する)。

// Shared cost-floor knobs (calibrate vs live; same defaults as time_series_momentum).
const (
	dcSlippageTicks   = 2.0
	dcMinEdgeMultiple = 2.0
)

// ta.closes が unexported なので同等物をここに置く。
func closesOf(cs []market.Candle) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = c.Close
	}
	return out
}

// SMA ベースの簡易 RSI(RSI-2 スクリーン用)。履歴不足は ok=false。
func rsiLast(cl []float64, n int) (float64, bool) {
	if n < 1 || len(cl) < n+1 {
		return 0, false
	}
	var gain, loss float64
	for i := len(cl) - n; i < len(cl); i++ {
		d := cl[i] - cl[i-1]
		if d > 0 {
			gain += d
		} else {
			loss += -d
		}
	}
	if gain+loss == 0 {
		return 50, true
	}
	rs := gain / (loss + 1e-12)
	return 100 - 100/(1+rs), true
}

func stdDevLast(cl []float64, n int) (mean, sd float64, ok bool) {
	if n < 2 || len(cl) < n {
		return 0, 0, false
	}
	w := cl[len(cl)-n:]
	var s float64
	for _, v := range w {
		s += v
	}
	mean = s / float64(n)
	var ss float64
	for _, v := range w {
		ss += (v - mean) * (v - mean)
	}
	return mean, math.Sqrt(ss / float64(n)), true
}

// 出口は ATR スケールで cfg.Exit の円幅は使わない。ATR が取れない(履歴不足)
// ときは黙って円幅に落ちず no_trade — 出口が銘柄のボラと無関係な値になる経路を残さないため。
func enterIfClears(in EvalInput, side order.Side, name config.StrategyName, reason string, slipTicks, minMult float64) Signal {
	switch in.Config.Entry.Direction {
	case config.DirectionBuyOnly:
		if side != order.SideBuy {
			return noTradeSignal(in, name, "direction_buy_only")
		}
	case config.DirectionSellOnly:
		if side != order.SideSell {
			return noTradeSignal(in, name, "direction_sell_only")
		}
	}
	atr := ta.ATR(in.CandlesDaily, atrExitPeriod)
	if atr <= 0 {
		return noTradeSignal(in, name, "no_atr")
	}
	sig := atrExitSignal(in, side, in.Summary.CurrentRate.Last, name, reason, atr)
	if sig.HoldingMode == "" {
		sig.HoldingMode = order.HoldingMultiday
	}
	sig.MaxHoldMinutes = applyStrategyMaxHold(in, name, sig.MaxHoldMinutes)
	floor := CostFloor{
		SpreadTicks:     in.Summary.CurrentRate.SpreadTicks,
		SlippageTicks:   slipTicks,
		MinEdgeMultiple: minMult,
		TickSize:        summaryTickSize(in),
	}
	return applyCostFloor(in, sig, floor)
}

const (
	maFast = 25
	maSlow = 75
)

type MACross struct{}

func (MACross) Name() config.StrategyName { return config.StrategyMACross }

func (s MACross) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, maSlow+1); !ok {
		return sig
	}
	d := in.CandlesDaily
	cl := closesOf(d)
	fast, slow := ta.SMA(cl, maFast), ta.SMA(cl, maSlow)
	pf, ps := ta.SMA(cl[:len(cl)-1], maFast), ta.SMA(cl[:len(cl)-1], maSlow)
	last := d[len(d)-1].Close
	var side order.Side
	switch {
	case pf <= ps && fast > slow && last > slow:
		side = order.SideBuy
	case pf >= ps && fast < slow && last < slow:
		side = order.SideSell
	default:
		return noTradeSignal(in, name, "no_cross")
	}
	return enterIfClears(in, side, name, "ma_cross", dcSlippageTicks, dcMinEdgeMultiple)
}

const (
	absLookback = 126 // ~6 months
	absTrendSMA = 200
)

type AbsMomentum struct{}

func (AbsMomentum) Name() config.StrategyName { return config.StrategyAbsMomentum }

func (s AbsMomentum) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, absTrendSMA+1); !ok {
		return sig
	}
	d := in.CandlesDaily
	cl := closesOf(d)
	sma := ta.SMA(cl, absTrendSMA)
	last := d[len(d)-1].Close
	past := cl[len(cl)-1-absLookback]
	var side order.Side
	switch {
	case last > sma && last > past:
		side = order.SideBuy
	case last < sma && last < past:
		side = order.SideSell
	default:
		return noTradeSignal(in, name, "no_abs_momentum")
	}
	return enterIfClears(in, side, name, "abs_momentum", dcSlippageTicks, dcMinEdgeMultiple)
}

const h52Window = 252

type High52wMomentum struct{}

func (High52wMomentum) Name() config.StrategyName { return config.StrategyHigh52wMomentum }

// 入口の判定を 1 箇所に置く(兄弟アームが**同じ関数**を呼ぶため)。
// 売り側(52週安値ブレイク)も元から書かれている — direction を開けるだけで
// 標本が取れるのはこのため。side=="" のとき理由を返す。
func high52wSideAt(d []market.Candle) (order.Side, string) {
	cl := closesOf(d)
	hi, lo, ok := ta.Donchian(d[:len(d)-1], h52Window)
	if !ok {
		return "", "insufficient_breakout_history"
	}
	sma := ta.SMA(cl, absTrendSMA)
	last := d[len(d)-1].Close
	switch {
	case last >= hi*0.99 && last > sma:
		return order.SideBuy, ""
	case last <= lo*1.01 && last < sma:
		return order.SideSell, ""
	}
	return "", "not_near_52w_extreme"
}

func (s High52wMomentum) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, h52Window+1); !ok {
		return sig
	}
	side, reason := high52wSideAt(in.CandlesDaily)
	if side == "" {
		return noTradeSignal(in, name, reason)
	}
	return enterIfClears(in, side, name, "high_52w_momentum", dcSlippageTicks, dcMinEdgeMultiple)
}

const dbWindow = 20

type DonchianBreakout struct{}

func (DonchianBreakout) Name() config.StrategyName { return config.StrategyDonchianBreakout }

func (s DonchianBreakout) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, dbWindow+1); !ok {
		return sig
	}
	d := in.CandlesDaily
	hi, lo, ok := ta.Donchian(d[:len(d)-1], dbWindow)
	if !ok {
		return noTradeSignal(in, name, "insufficient_breakout_history")
	}
	last := d[len(d)-1].Close
	var side order.Side
	switch {
	case last >= hi:
		side = order.SideBuy
	case last <= lo:
		side = order.SideSell
	default:
		return noTradeSignal(in, name, "no_breakout")
	}
	return enterIfClears(in, side, name, "donchian_breakout", dcSlippageTicks, dcMinEdgeMultiple)
}

const (
	atrN        = 14
	atrTrendSMA = 100
)

type ATRBreakout struct{}

func (ATRBreakout) Name() config.StrategyName { return config.StrategyATRBreakout }

func (s ATRBreakout) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, atrTrendSMA+1); !ok {
		return sig
	}
	d := in.CandlesDaily
	cl := closesOf(d)
	atr := ta.ATR(d, atrN)
	if atr <= 0 {
		return noTradeSignal(in, name, "no_atr")
	}
	sma := ta.SMA(cl, atrTrendSMA)
	prevClose := d[len(d)-2].Close
	last := d[len(d)-1].Close
	var side order.Side
	switch {
	case last > prevClose+atr && last > sma:
		side = order.SideBuy
	case last < prevClose-atr && last < sma:
		side = order.SideSell
	default:
		return noTradeSignal(in, name, "no_range_expansion")
	}
	return enterIfClears(in, side, name, "atr_breakout", dcSlippageTicks, dcMinEdgeMultiple)
}

const (
	rsiN     = 2
	rsiBuyTh = 10.0
	rsiSlTh  = 90.0
)

type RSI2Reversion struct{}

func (RSI2Reversion) Name() config.StrategyName { return config.StrategyRSI2Reversion }

func (s RSI2Reversion) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, absTrendSMA+1); !ok {
		return sig
	}
	d := in.CandlesDaily
	cl := closesOf(d)
	r, ok := rsiLast(cl, rsiN)
	if !ok {
		return noTradeSignal(in, name, "no_rsi")
	}
	sma := ta.SMA(cl, absTrendSMA)
	last := d[len(d)-1].Close
	var side order.Side
	switch {
	case last > sma && r < rsiBuyTh:
		side = order.SideBuy
	case last < sma && r > rsiSlTh:
		side = order.SideSell
	default:
		return noTradeSignal(in, name, "no_reversion_setup")
	}
	return enterIfClears(in, side, name, "rsi2_reversion", dcSlippageTicks, 1.0)
}

const (
	bbN = 20
	bbK = 2.0
)

type BollingerReversion struct{}

func (BollingerReversion) Name() config.StrategyName { return config.StrategyBollingerReversion }

func (s BollingerReversion) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, bbN); !ok {
		return sig
	}
	d := in.CandlesDaily
	cl := closesOf(d)
	mean, sd, ok := stdDevLast(cl, bbN)
	if !ok || sd <= 0 {
		return noTradeSignal(in, name, "no_band")
	}
	last := d[len(d)-1].Close
	var side order.Side
	switch {
	case last < mean-bbK*sd:
		side = order.SideBuy
	case last > mean+bbK*sd:
		side = order.SideSell
	default:
		return noTradeSignal(in, name, "inside_band")
	}
	return enterIfClears(in, side, name, "bollinger_reversion", dcSlippageTicks, 1.0)
}

const (
	hvpLookback = 50  // a GKM "high-volume day" = today's volume is the max of this window
	hvpMinMult  = 2.0 // and at least this multiple of the trailing average
	hvpTrendSMA = 100 // only in a non-broken trend (avoid high-volume capitulation = BNF's turf)
)

type HighVolumePremium struct{}

func (HighVolumePremium) Name() config.StrategyName { return config.StrategyHighVolumePremium }

func (s HighVolumePremium) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, hvpTrendSMA+1); !ok {
		return sig
	}
	d := in.CandlesDaily
	last := d[len(d)-1]
	prior := d[len(d)-hvpLookback : len(d)-1] // prior bars only (exclude today)
	maxVol, sumVol := 0.0, 0.0
	for _, c := range prior {
		if c.Volume > maxVol {
			maxVol = c.Volume
		}
		sumVol += c.Volume
	}
	avg := sumVol / float64(len(prior))
	if avg <= 0 {
		return noTradeSignal(in, name, "no_volume")
	}
	if !(last.Volume > maxVol && last.Volume >= hvpMinMult*avg) {
		return noTradeSignal(in, name, "not_high_volume_day")
	}
	// keep distinct from BNF capitulation: only when the trend is not broken down.
	if last.Close < ta.SMA(closesOf(d), hvpTrendSMA) {
		return noTradeSignal(in, name, "below_trend")
	}
	return enterIfClears(in, order.SideBuy, name, "high_volume_premium", dcSlippageTicks, 1.0)
}

const (
	peadVolWindow = 60  // trailing daily-return vol window
	peadJumpSigma = 2.5 // a "surprise" = today's return >= this many sigma
	peadVolMult   = 3.0 // confirmed by volume >= this x the 25-day average
)

type PostJumpDrift struct{}

func (PostJumpDrift) Name() config.StrategyName { return config.StrategyPostJumpDrift }

func (s PostJumpDrift) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, peadVolWindow+2); !ok {
		return sig
	}
	d := in.CandlesDaily
	prev := d[len(d)-2].Close
	last := d[len(d)-1].Close
	if prev <= 0 {
		return noTradeSignal(in, name, "bad_prev_close")
	}
	ret := last/prev - 1
	var rs []float64
	for i := len(d) - peadVolWindow; i < len(d); i++ {
		if p0 := d[i-1].Close; p0 > 0 {
			rs = append(rs, d[i].Close/p0-1)
		}
	}
	if len(rs) < 20 {
		return noTradeSignal(in, name, "insufficient_returns")
	}
	sigma := stdev(rs) // screen.go と同一式(母集団σ)
	if sigma <= 0 {
		return noTradeSignal(in, name, "no_vol")
	}
	volAvg := avgVolume(d, 25)
	if volAvg <= 0 {
		return noTradeSignal(in, name, "no_volume")
	}
	volRatio := d[len(d)-1].Volume / volAvg
	// positive surprise: a big up-jump on a volume spike → bet on continuation.
	if !(ret >= peadJumpSigma*sigma && volRatio >= peadVolMult) {
		return noTradeSignal(in, name, "no_jump_event")
	}
	return enterIfClears(in, order.SideBuy, name, "post_jump_drift", dcSlippageTicks, 1.0)
}

const gapPct = 0.03

type GapReversion struct{}

func (GapReversion) Name() config.StrategyName { return config.StrategyGapReversion }

func (s GapReversion) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, 2); !ok {
		return sig
	}
	d := in.CandlesDaily
	prevClose := d[len(d)-2].Close
	open := d[len(d)-1].Open
	if prevClose <= 0 {
		return noTradeSignal(in, name, "bad_prev_close")
	}
	gap := (open - prevClose) / prevClose
	var side order.Side
	switch {
	case gap <= -gapPct:
		side = order.SideBuy
	case gap >= gapPct:
		side = order.SideSell
	default:
		return noTradeSignal(in, name, "no_gap")
	}
	return enterIfClears(in, side, name, "gap_reversion", dcSlippageTicks, 1.0)
}
