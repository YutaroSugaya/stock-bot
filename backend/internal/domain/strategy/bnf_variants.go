package strategy

import (
	"fmt"
	"math"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/ta"
)

// bnf ファミリーの新入口 2 つ。
//
// 🛑 **閾値はどちらも新しく選ばない。** −12% / 1.5× / 2.0×ATR / 1.0・1.5×ATR / 10 暦日は
// 全部 bnf.go の定数をそのまま参照する(数字を写すと定数を動かしたとき静かにずれる)。
// 自由度は A が「判定時点を前日終値から場中へ移す」の 1 点、B が「建てる時点の現在値が
// 前日終値を割っていたら見送る」のゲート 1 つだけ。
//
// 🛑 **2 件とも負けた日に出てきた仮説**なので、標本を見る前に登録した
// (同書)。既存 bnf の建玉を「建値 ≥ 前日終値 / < 前日終値」に割って先に効き目を見る
// ことは禁じている。
//
// ⚠ A は場中に建てる(bnf は寄りで建てる)ので、paper の約定価格は観測ティック
// (3〜6 秒間隔)そのもの。事前登録が「板の後」と書いていた依存を承知で入れた。
// `Ticker.Stale`(前日終値 fallback)は上流(bundle)が落とすので
// ここには来ない。

// ---- 共有: 出口幾何(bnf.go の Evaluate と 1 ビットも違えない) ----------------

// bnfCappedExit は capped 脚の出口: TP = 25 日線まで / SL = 2.0×ATR / MaxHold 10 営業日目の 14:50。
func bnfCappedExit(in EvalInput, sig Signal, sma, atr float64) Signal {
	stopATR := tuning(in, "bnf_stop_atr", bnfStopATR)
	sig.TakeProfitJPY, sig.StopLossJPY = bnfReversionExitJPY(sig.EntryPrice, sma, stopATR, atr)
	sig.MaxHoldMinutes = closeAlignedMaxHold(in, bnfMaxHoldBusinessDays)
	sig.RatchetArmJPY = 0
	sig.RatchetGivebackJPY = 0
	return applyCostFloor(in, sig, bnfCostFloor(in))
}

// bnfTrailExit は trail 脚の出口: TP なし / SL = 2.0×ATR / ratchet arm 1.0×ATR・giveback 1.5×ATR。
func bnfTrailExit(in EvalInput, sig Signal, atr float64) Signal {
	stopATR := tuning(in, "bnf_stop_atr", bnfStopATR)
	sig.TakeProfitJPY = 0 // uncapped
	sig.StopLossJPY = stopATR * atr
	sig.MaxHoldMinutes = closeAlignedMaxHold(in, bnfMaxHoldBusinessDays)
	// ATR は元から円/株なので、そのまま倍率を掛けるだけ。
	sig.RatchetArmJPY = bnfTrailArmATR * atr
	sig.RatchetGivebackJPY = bnfTrailGiveATR * atr
	return applyCostFloor(in, sig, bnfCostFloor(in))
}

// currentLast は建てる時点の現在値。0 以下は「価格が無い」(評価しない)。
func currentLast(in EvalInput) float64 {
	if in.Summary == nil {
		return 0
	}
	return in.Summary.CurrentRate.Last
}

// ---- A. bnf_day2_reversion / _trail ------------------------------------------------

// bnfDay2Enter は day2 の入口。前日の確定足で「出来高 ≥ 1.5× かつ 乖離 > −12%(まだ
// パニックではない)」、当日の場中で「現在値の乖離 ≤ −12%」。25 日線は前日までの確定足
// (形成中の当日を含めると自己参照になる)。
func bnfDay2Enter(in EvalInput, name config.StrategyName) (sig Signal, sma, atr float64, ok bool) {
	if in.Config == nil || in.Summary == nil {
		return noTradeSignal(in, name, "no_config_or_summary"), 0, 0, false
	}
	d := ConfirmedDailyBefore(in.CandlesDaily, in.Now)
	if len(d) < bnfSMA+1 {
		return noTradeSignal(in, name, "insufficient_daily_history"), 0, 0, false
	}
	sma = ta.SMA(closesOf(d), bnfSMA)
	if sma <= 0 {
		return noTradeSignal(in, name, "no_sma"), 0, 0, false
	}
	volAvg := avgVolume(d, bnfVolSMA)
	if volAvg <= 0 {
		return noTradeSignal(in, name, "no_volume"), 0, 0, false
	}
	prev := d[len(d)-1]
	devThreshold := tuning(in, "bnf_dev", bnfDevThreshold)
	volThreshold := tuning(in, "bnf_vol", bnfVolRatio)
	prevDev := (prev.Close - sma) / sma
	if !(prev.Volume/volAvg >= volThreshold && prevDev > devThreshold) {
		return noTradeSignal(in, name, "no_day2_setup"), 0, 0, false
	}
	// BNF と同じく急落を買う — long only。
	if in.Config.Entry.Direction == config.DirectionSellOnly {
		return noTradeSignal(in, name, "direction_sell_only"), 0, 0, false
	}
	last := currentLast(in)
	if last <= 0 {
		return noTradeSignal(in, name, "no_price"), 0, 0, false
	}
	if (last-sma)/sma > devThreshold {
		return noTradeSignal(in, name, "no_intraday_panic"), 0, 0, false
	}
	atr = ta.ATR(d, bnfATRPeriod)
	if atr <= 0 {
		return noTradeSignal(in, name, "no_atr"), 0, 0, false
	}
	sig = configExitSignal(in, order.SideBuy, last, name, "bnf_day2_panic_reversion")
	if sig.HoldingMode == "" {
		sig.HoldingMode = order.HoldingMultiday
	}
	return sig, sma, atr, true
}

// BNFDay2Reversion は day2 の capped 脚。
type BNFDay2Reversion struct{}

func (BNFDay2Reversion) Name() config.StrategyName { return config.StrategyBNFDay2Reversion }

func (s BNFDay2Reversion) Evaluate(in EvalInput) Signal {
	sig, sma, atr, ok := bnfDay2Enter(in, s.Name())
	if !ok {
		return sig
	}
	return bnfCappedExit(in, sig, sma, atr)
}

// Screen は前日条件だけをミラーする(当日の場中条件は日足からは判定できない)。
// Score は bnf と同じ min(乖離/閾値, 出来高/閾値): 乖離が −12% に近い候補ほど当日に
// 割る確率が高いので上に並ぶ。新しい定数は使わない。
func (BNFDay2Reversion) Screen(symbol string, d []market.Candle) Candidate {
	c := Candidate{Symbol: symbol, Strategy: config.StrategyBNFDay2Reversion}
	if len(d) < bnfSMA+1 {
		c.Detail = "insufficient_history"
		return c
	}
	sma := ta.SMA(closesOf(d), bnfSMA)
	if sma <= 0 {
		c.Detail = "no_sma"
		return c
	}
	volAvg := avgVolume(d, bnfVolSMA)
	if volAvg <= 0 {
		c.Detail = "no_volume"
		return c
	}
	last := d[len(d)-1]
	dev := (last.Close - sma) / sma
	volRatio := last.Volume / volAvg
	notYet := dev > bnfDevThreshold
	c.Score = math.Min(dev/bnfDevThreshold, volRatio/bnfVolRatio)
	c.Triggered = volRatio >= bnfVolRatio && notYet
	c.Detail = fmt.Sprintf("dev %+.1f%%(前日・未達) / vol %.2fx / 当日 -12%% 割れで建てる", dev*100, volRatio)
	c.Conditions = []Condition{
		{Label: "出来高1.5倍以上(前日)", Met: volRatio >= bnfVolRatio, Got: fmt.Sprintf("%.2fx", volRatio)},
		{Label: "25日線乖離が-12%より上(前日はまだパニックでない)", Met: notYet, Got: fmt.Sprintf("%+.1f%%", dev*100)},
	}
	// 計画ストップ幅は建値に依らず 2.0×ATR(枠を配る前の重さ判定用・bnf と同じ関数)。
	if atr := ta.ATR(d, bnfATRPeriod); atr > 0 {
		_, c.StopLossJPY = bnfReversionExitJPY(last.Close, sma, bnfStopATR, atr)
	}
	return c
}

// BNFDay2ReversionTrail は day2 の uncapped 兄弟(入口同一・出口だけ ratchet)。
type BNFDay2ReversionTrail struct{}

func (BNFDay2ReversionTrail) Name() config.StrategyName {
	return config.StrategyBNFDay2ReversionTrail
}

func (s BNFDay2ReversionTrail) Evaluate(in EvalInput) Signal {
	sig, _, atr, ok := bnfDay2Enter(in, s.Name())
	if !ok {
		return sig
	}
	return bnfTrailExit(in, sig, atr)
}

func (BNFDay2ReversionTrail) Screen(symbol string, d []market.Candle) Candidate {
	return trailDetail(relabel(BNFDay2Reversion{}.Screen(symbol, d), config.StrategyBNFDay2ReversionTrail))
}

// ---- B. bnf_stabilized_reversion / _trail ----------------------------------------

// bnfStabilizedEnter は bnf の入口(bnfEnter・同一関数)に「現在値 < 前日終値なら見送る」を
// 足したもの。ゲートは入口条件の**後**に置く — パニックでない日に still_falling を数えると
// signal_rejections の分布が bnf と比較できなくなる。閾値は 0(前日終値を割っているか否か)。
//
// bnf の screen は前日終値の条件が続く限り毎日発火するので、「翌日も下落 → 次の日に
// 下げ止まり → 建てる」は窓の長さという新しいパラメータ無しに成立する。
func bnfStabilizedEnter(in EvalInput, name config.StrategyName) (sig Signal, sma, atr float64, ok bool) {
	sig, sma, atr, ok = bnfEnter(in, name)
	if !ok {
		return sig, 0, 0, false
	}
	last := currentLast(in)
	if last <= 0 {
		return noTradeSignal(in, name, "no_price"), 0, 0, false
	}
	if prevClose := in.CandlesDaily[len(in.CandlesDaily)-1].Close; last < prevClose {
		return noTradeSignal(in, name, "still_falling"), 0, 0, false
	}
	return sig, sma, atr, true
}

// BNFStabilizedReversion は下げ止まり確認型の capped 脚。
type BNFStabilizedReversion struct{}

func (BNFStabilizedReversion) Name() config.StrategyName {
	return config.StrategyBNFStabilizedReversion
}

func (s BNFStabilizedReversion) Evaluate(in EvalInput) Signal {
	name := s.Name()
	sig, sma, atr, ok := bnfStabilizedEnter(in, name)
	if !ok {
		return sig
	}
	if atr <= 0 {
		return noTradeSignal(in, name, "no_atr")
	}
	return bnfCappedExit(in, sig, sma, atr)
}

// Screen は bnf と**完全に同一**(ゲートは場中の現在値で決まり、日足からは判定できない)。
func (BNFStabilizedReversion) Screen(symbol string, d []market.Candle) Candidate {
	c := relabel(BNFReversion{}.Screen(symbol, d), config.StrategyBNFStabilizedReversion)
	if c.Detail != "" && c.Triggered {
		c.Detail += " / 前日終値割れの間は見送る"
	}
	return c
}

// BNFStabilizedReversionTrail は下げ止まり確認型の uncapped 兄弟。
type BNFStabilizedReversionTrail struct{}

func (BNFStabilizedReversionTrail) Name() config.StrategyName {
	return config.StrategyBNFStabilizedReversionTrail
}

func (s BNFStabilizedReversionTrail) Evaluate(in EvalInput) Signal {
	name := s.Name()
	sig, _, atr, ok := bnfStabilizedEnter(in, name)
	if !ok {
		return sig
	}
	if atr <= 0 {
		return noTradeSignal(in, name, "no_atr")
	}
	return bnfTrailExit(in, sig, atr)
}

func (BNFStabilizedReversionTrail) Screen(symbol string, d []market.Candle) Candidate {
	return trailDetail(relabel(BNFStabilizedReversion{}.Screen(symbol, d), config.StrategyBNFStabilizedReversionTrail))
}

// trailDetail は兄弟の行を人間が区別できるようにする(同じ行が 2 本並ぶ)。
func trailDetail(c Candidate) Candidate {
	if c.Detail != "" && c.Triggered {
		c.Detail += " / 出口=ATRトレール(利伸ばし)"
	}
	return c
}
