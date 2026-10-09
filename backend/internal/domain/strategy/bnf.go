package strategy

import (
	"math"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/ta"
)

// BNF(小手川隆)パニック逆張り。capped / trail の 2 アームは入口(25日線から -12% の急落 ×
// 出来高 1.5 倍)を共有し出口だけが違う — 変数を出口 1 点に絞らないと勝敗を入口に帰属できない。
// 深い急落ゲート + パニック出来高ゲートが、大型株を「事象で選ばれた mover」に変える点が、
// 棄却済みの rsi2 / bollinger 逆張り(浅い閾値・出来高ゲート無し)との差。

const (
	bnfSMA          = 25
	bnfVolSMA       = 25
	bnfDevThreshold = -0.12 // enter when close is >=12% below the 25-day MA (sharp crash)
	bnfVolRatio     = 1.5   // confirm with >=1.5x the 25-day average volume (panic selling)
	bnfStopATR      = 2.0   // hard stop at 2.0·ATR below entry (Wilder の 2N ストップ)
	// MaxHold は **10 営業日目の引け前(14:50)**(損益に依らずその時刻で決済)。
	bnfMaxHoldBusinessDays = 10
	bnfATRPeriod           = 14
	bnfTrailArmATR         = 1.0 // arm the ratchet after +1·ATR favourable
	bnfTrailGiveATR        = 1.5 // exit on 1.5·ATR giveback from the running peak
)

// backtest の robustness sweep 用 override。無ければ事前登録の定数。
func tuning(in EvalInput, key string, def float64) float64 {
	if in.Config != nil && in.Config.Tuning != nil {
		if v, ok := in.Config.Tuning[key]; ok {
			return v
		}
	}
	return def
}

func avgVolume(cs []market.Candle, n int) float64 {
	if n <= 0 || len(cs) < n {
		return 0
	}
	sum := 0.0
	for _, c := range cs[len(cs)-n:] {
		sum += c.Volume
	}
	return sum / float64(n)
}

// 共有の入口ゲート。ok=false のとき sig は伝播させる NO_TRADE(出口は呼び手が組む)。
func bnfEnter(in EvalInput, name config.StrategyName) (sig Signal, sma, atr float64, ok bool) {
	if in.Config == nil || in.Summary == nil {
		return noTradeSignal(in, name, "no_config_or_summary"), 0, 0, false
	}
	d := in.CandlesDaily
	if len(d) < bnfSMA+1 {
		return noTradeSignal(in, name, "insufficient_daily_history"), 0, 0, false
	}
	cl := closesOf(d)
	sma = ta.SMA(cl, bnfSMA)
	last := d[len(d)-1].Close
	if sma <= 0 {
		return noTradeSignal(in, name, "no_sma"), 0, 0, false
	}
	dev := (last - sma) / sma
	volAvg := avgVolume(d, bnfVolSMA)
	if volAvg <= 0 {
		return noTradeSignal(in, name, "no_volume"), 0, 0, false
	}
	volRatio := d[len(d)-1].Volume / volAvg
	devThreshold := tuning(in, "bnf_dev", bnfDevThreshold)
	volThreshold := tuning(in, "bnf_vol", bnfVolRatio)
	if !(dev <= devThreshold && volRatio >= volThreshold) {
		return noTradeSignal(in, name, "no_panic_crash"), 0, 0, false
	}
	// BNF buys the crash — long only.
	if in.Config.Entry.Direction == config.DirectionSellOnly {
		return noTradeSignal(in, name, "direction_sell_only"), 0, 0, false
	}
	atr = ta.ATR(d, bnfATRPeriod)
	sig = configExitSignal(in, order.SideBuy, last, name, "bnf_panic_reversion")
	if sig.HoldingMode == "" {
		sig.HoldingMode = order.HoldingMultiday
	}
	return sig, sma, atr, true
}

func bnfCostFloor(in EvalInput) CostFloor {
	return CostFloor{
		SpreadTicks:     in.Summary.CurrentRate.SpreadTicks,
		SlippageTicks:   dcSlippageTicks,
		MinEdgeMultiple: 1.0,
		TickSize:        summaryTickSize(in),
	}
}

// BNFReversion is the CAPPED arm: take-profit at the 25-MA reversion target.
type BNFReversion struct{}

// Name implements Strategy.
func (BNFReversion) Name() config.StrategyName { return config.StrategyBNFReversion }

// Evaluate implements Strategy.
func (s BNFReversion) Evaluate(in EvalInput) Signal {
	name := s.Name()
	sig, sma, atr, ok := bnfEnter(in, name)
	if !ok {
		return sig
	}
	if atr <= 0 {
		return noTradeSignal(in, name, "no_atr")
	}
	// 出口は bnf_variants.go の共有ヘルパー(day2 / stabilized と 1 ビットも違えない)。
	return bnfCappedExit(in, sig, sma, atr)
}

// capped-BNF の出口幾何(円/株)。SL は 2.0·ATR(建値の固定率ではない)
// — 固定率は銘柄のボラに追従せず、高ボラ銘柄では TP に届く前に必ず刈られていた。
func bnfReversionExitJPY(entry, sma25, stopATR, atr float64) (tpJPY, slJPY float64) {
	tpJPY = math.Max(0, sma25-entry) // revert up to the 25-MA
	slJPY = stopATR * atr
	return tpJPY, slJPY
}

// 上の幾何の公開版(advisor / scan 用)。片方だけ動かすと見積りが黙ってズレる — 必ず cmd/advise と同時に。
func BNFReversionExit(entry, sma25, stopATR, atr float64) (tpJPY, slJPY float64) {
	return bnfReversionExitJPY(entry, sma25, stopATR, atr)
}

// 事前登録の BNF 定数の公開ミラー(advisor が同一パラメータで screen / quote するため)。
const (
	BNFSMAPeriod      = bnfSMA       // 25-day MA: entry deviation + reversion (TP) target
	BNFATRPeriod      = bnfATRPeriod // ATR(14): 出口幅のスケール
	DefaultBNFStopATR = bnfStopATR   // 2.0·ATR hard stop below entry
)

// BNFReversion の上側ミラー(急騰の売り)。エッジが下落側の流動性供給に固有かを問う対照。
// ⚠ 貸株料を未モデル化なので、売り側の net はどれだけ良く出ても楽観側にズレている。
type BNFEuphoriaShort struct{}

func (BNFEuphoriaShort) Name() config.StrategyName { return config.StrategyBNFEuphoriaShort }

func (s BNFEuphoriaShort) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, bnfSMA+1); !ok {
		return sig
	}
	d := in.CandlesDaily
	cl := closesOf(d)
	sma := ta.SMA(cl, bnfSMA)
	last := d[len(d)-1].Close
	if sma <= 0 {
		return noTradeSignal(in, name, "no_sma")
	}
	dev := (last - sma) / sma
	volAvg := avgVolume(d, bnfVolSMA)
	if volAvg <= 0 {
		return noTradeSignal(in, name, "no_volume")
	}
	volRatio := d[len(d)-1].Volume / volAvg
	devUp := -tuning(in, "bnf_dev", bnfDevThreshold) // mirror: +0.12 by default
	volThreshold := tuning(in, "bnf_vol", bnfVolRatio)
	if !(dev >= devUp && volRatio >= volThreshold) {
		return noTradeSignal(in, name, "no_euphoria_spike")
	}
	if in.Config.Entry.Direction == config.DirectionBuyOnly {
		return noTradeSignal(in, name, "direction_buy_only")
	}
	atr := ta.ATR(d, bnfATRPeriod)
	if atr <= 0 {
		return noTradeSignal(in, name, "no_atr")
	}
	stopATR := tuning(in, "bnf_stop_atr", bnfStopATR)
	sig := configExitSignal(in, order.SideSell, last, name, "bnf_euphoria_reversion")
	if sig.HoldingMode == "" {
		sig.HoldingMode = order.HoldingMultiday
	}
	sig.TakeProfitJPY = math.Max(0, last-sma) // SELL: target = revert DOWN to the 25-MA
	sig.StopLossJPY = stopATR * atr
	sig.MaxHoldMinutes = closeAlignedMaxHold(in, bnfMaxHoldBusinessDays)
	sig.RatchetArmJPY = 0
	sig.RatchetGivebackJPY = 0
	return applyCostFloor(in, sig, bnfCostFloor(in))
}

// uncapped アーム: 入口は同一で、出口だけ ATR ratchet(利伸ばし)。A/B の変数は出口のみ。
type BNFReversionTrail struct{}

func (BNFReversionTrail) Name() config.StrategyName { return config.StrategyBNFReversionTrail }

func (s BNFReversionTrail) Evaluate(in EvalInput) Signal {
	name := s.Name()
	sig, _, atr, ok := bnfEnter(in, name)
	if !ok {
		return sig
	}
	if atr <= 0 {
		return noTradeSignal(in, name, "no_atr")
	}
	// 出口は bnf_variants.go の共有ヘルパー(day2 / stabilized と 1 ビットも違えない)。
	return bnfTrailExit(in, sig, atr)
}
