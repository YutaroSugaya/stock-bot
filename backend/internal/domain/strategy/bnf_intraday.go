package strategy

import (
	"math"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/ta"
)

// BNF パニック逆張りの日中版(前日確定足のパニック → 当日の5分足反転で押し目買い・同日決済)。
// 🛑 未証明: forward paper で測る(バックテストは使わない)。
// それまで live allowlist に入れてはいけない。
// 🛑 凍結値(30% / 当日安値 −1 tick / 2% / 3 本 / 14:50)は測定中に動かさない。
const (
	bnfiMinBars    = 3    // completed 5m bars required (skips the opening auction chaos)
	bnfiTPFrac     = 0.3  // take the first 30% of the gap back toward the 25-MA
	bnfiSLCapPct   = 0.02 // stop no wider than 2% below entry (day-trade risk unit)
	bnfiMaxHoldMin = 300  // ceiling only; the gate clamps to MinutesUntilForceFlat
	// **買うのは前場(〜11:30)だけ**。揃わなければその日は見送り。
	// 後場に買うと、時間切れで閉じた直後に買い直して残り数十分で強制決済される形になる。
	// 値は新しく選んだ数字ではなく東証の前場の終わり(JST・DST なし)。
	bnfiEntryCutoffMin = 11*60 + 30
)

// JST は DST が無いので固定オフセット(domain を純粋・決定的に保つ)。
var jstZone = clock.JST

type BNFIntradayReversion struct{}

func (BNFIntradayReversion) Name() config.StrategyName { return config.StrategyBNFIntradayReversion }

// 日足ゲートは BNF と同一。意味だけが違う(BNF は翌朝から数日保有、こちらは翌場を日計り)。
func (BNFIntradayReversion) Screen(symbol string, d []market.Candle) Candidate {
	c := BNFReversion{}.Screen(symbol, d)
	c.Strategy = config.StrategyBNFIntradayReversion
	return c
}

func (s BNFIntradayReversion) Evaluate(in EvalInput) Signal {
	sig, tpJPY, ok := bnfIntradayEnter(in, s.Name())
	if !ok {
		return sig
	}
	// capped: TP = 25 日線への戻りの 30%(距離 A)。両脚 broker 側 OCO。
	sig.TakeProfitJPY = tpJPY
	return bnfIntradayCostFloor(in, sig)
}

// bnfIntradayEnter は capped / trail 兄弟が**共有する入口**(bnf_variants.go の bnfEnter と同じ
// 作法)。返す Signal は出口の TP / ratchet を持たない — 呼び手が表どおりに 1 点だけ
// 差し替える。tpJPY は「25 日線への戻りの 30%」= 距離 A で、capped では TP、trail では
// ratchet の arm になる(giveback は 1.5×A)。
//
// 🛑 入口判定・SL・MaxHold・HoldingMode はここで確定し、兄弟間で 1 ビットも違わない
// (`TestBNFIntradayTrail_EntersExactlyWhenCappedDoes` が固定)。
func bnfIntradayEnter(in EvalInput, name config.StrategyName) (sig Signal, tpJPY float64, ok bool) {
	if in.Config == nil || in.Summary == nil {
		return noTradeSignal(in, name, "no_config_or_summary"), 0, false
	}
	if t := in.Now.In(jstZone); t.Hour()*60+t.Minute() >= bnfiEntryCutoffMin {
		return noTradeSignal(in, name, "outside_morning_entry_window"), 0, false
	}

	// パニック判定は当日 JST セッションより前の確定足のみ(形成中の日足でゲートが動かないように)。
	daily := ConfirmedDailyBefore(in.CandlesDaily, in.Now)
	if len(daily) < bnfSMA+1 {
		return noTradeSignal(in, name, "insufficient_daily_history"), 0, false
	}
	sma := ta.SMA(closesOf(daily), bnfSMA)
	if sma <= 0 {
		return noTradeSignal(in, name, "no_sma"), 0, false
	}
	volAvg := avgVolume(daily, bnfVolSMA)
	if volAvg <= 0 {
		return noTradeSignal(in, name, "no_volume"), 0, false
	}
	prev := daily[len(daily)-1]
	dev := (prev.Close - sma) / sma
	volRatio := prev.Volume / volAvg
	if !(dev <= tuning(in, "bnf_dev", bnfDevThreshold) && volRatio >= tuning(in, "bnf_vol", bnfVolRatio)) {
		return noTradeSignal(in, name, "no_prev_day_panic"), 0, false
	}
	if in.Config.Entry.Direction == config.DirectionSellOnly {
		return noTradeSignal(in, name, "direction_sell_only"), 0, false // long-only, like BNF
	}

	last := in.Summary.CurrentRate.Last
	if last <= 0 {
		return noTradeSignal(in, name, "no_price"), 0, false
	}
	// Reversion room: once price is back at the 25-MA the bounce is spent.
	if last >= sma {
		return noTradeSignal(in, name, "no_reversion_room"), 0, false
	}

	// 25日線まで戻っていれば反発は使い切り。
	m5 := completedBars(in.Candles5m, in.Now)
	if len(m5) < int(tuning(in, "bnfi_min_bars", bnfiMinBars)) || len(m5) < 2 {
		return noTradeSignal(in, name, "insufficient_intraday_bars"), 0, false
	}
	cur, prior := m5[len(m5)-1], m5[len(m5)-2]
	if !(cur.Close > cur.Open && cur.Close > prior.High) {
		return noTradeSignal(in, name, "no_intraday_reversal"), 0, false
	}

	// 出口は建玉時に凍結し両脚 broker 側 OCO。SL は「今日の安値の1つ下」で、bnfi_sl_cap で上限を切る
	// (呼値はその1つ下の値段を作るためだけに使い、幅は円のまま)。
	tick := market.TickSizeOf(evalSymbol(in), last)
	tpJPY = math.Max(0, (sma-last)*tuning(in, "bnfi_tp_frac", bnfiTPFrac))
	todayLow := m5[0].Low
	for _, c := range m5 {
		if c.Low < todayLow {
			todayLow = c.Low
		}
	}
	slJPY := math.Max(0, last-(todayLow-tick))
	if cap := last * tuning(in, "bnfi_sl_cap", bnfiSLCapPct); slJPY > cap {
		slJPY = cap
	}

	sig = configExitSignal(in, order.SideBuy, last, name, "bnfi_panic_bounce")
	sig.TakeProfitJPY = 0 // 出口の 1 点は呼び手(capped / trail)が置く
	sig.RatchetArmJPY, sig.RatchetGivebackJPY = 0, 0
	sig.StopLossJPY = slJPY
	if sig.MaxHoldMinutes <= 0 || sig.MaxHoldMinutes > bnfiMaxHoldMin {
		sig.MaxHoldMinutes = bnfiMaxHoldMin
	}
	sig.HoldingMode = order.HoldingIntraday // day-trade by definition, not config default
	return sig, tpJPY, true
}

// bnfIntradayCostFloor は兄弟共通の床。`applyCostFloor` は max(TP, RatchetArm) を
// 当てるので、capped の TP と trail の arm が同じ距離 A なら床の判定も同じになる。
func bnfIntradayCostFloor(in EvalInput, sig Signal) Signal {
	return applyCostFloor(in, sig, CostFloor{
		SpreadTicks:     in.Summary.CurrentRate.SpreadTicks,
		SlippageTicks:   dcSlippageTicks,
		MinEdgeMultiple: 1.0,
		TickSize:        summaryTickSize(in),
	})
}

// BNFIntradayReversionTrail は日中版の uncapped 兄弟。
//
// 自由度は「TP を ratchet に置き換える」の 1 点だけ:
//   - TP なし
//   - SL 同じ(当日安値 −1 tick・上限 2%)
//   - ratchet arm = A(= capped の TP 距離 = 25 日線への戻りの 30%)
//   - ratchet giveback = 1.5 × A(多日 trail の giveback ÷ arm = 1.5 の比を借りる・新しい数字ではない)
//   - 14:50 強制 / MaxHold 同じ / intraday
//
// 日足 ATR を使わないのは、日中の値幅より大きくて一度も arm しない別物になるため。
// 対照は capped 版(ペア差)。
type BNFIntradayReversionTrail struct{}

func (BNFIntradayReversionTrail) Name() config.StrategyName {
	return config.StrategyBNFIntradayReversionTrail
}

func (s BNFIntradayReversionTrail) Evaluate(in EvalInput) Signal {
	sig, a, ok := bnfIntradayEnter(in, s.Name())
	if !ok {
		return sig
	}
	sig.TakeProfitJPY = 0 // uncapped
	sig.RatchetArmJPY = a
	sig.RatchetGivebackJPY = a * (bnfTrailGiveATR / bnfTrailArmATR)
	return bnfIntradayCostFloor(in, sig)
}

// Screen は capped に委譲してラベルだけ張り替える(独自実装するとスクリーンと入口がズレる)。
func (BNFIntradayReversionTrail) Screen(symbol string, d []market.Candle) Candidate {
	return trailDetail(relabel(BNFIntradayReversion{}.Screen(symbol, d), config.StrategyBNFIntradayReversionTrail))
}

// 段階ウォッチ Tier A の判定(純粋)。前日条件は bnf_intraday_reversion と同一・既定閾値。
// 低頻度の一括ポーリングでは 1分足の値幅が系統的に潰れる(TACHIBANA_API_NOTES §1.6-1)ので、
// 日中戦略が測定対象にする銘柄だけ高頻度レーンに乗せるための選別。
func IntradayHotCandidate(symbol string, daily []market.Candle, now time.Time) bool {
	confirmed := ConfirmedDailyBefore(daily, now)
	return (BNFReversion{}).Screen(symbol, confirmed).Triggered
}

// JST 当日より厳密に前の日足だけを返す(形成中の当日足を落とす)。exported なのは、日中リプレイに
// 日足コンテキストを注入するバックテストエンジンに**同じ** no-look-ahead カットを使わせるため。
func ConfirmedDailyBefore(daily []market.Candle, now time.Time) []market.Candle {
	y, m, d := now.In(jstZone).Date()
	cutoff := time.Date(y, m, d, 0, 0, 0, 0, jstZone)
	out := make([]market.Candle, 0, len(daily))
	for _, c := range daily {
		if c.OpenTime.In(jstZone).Before(cutoff) {
			out = append(out, c)
		}
	}
	return out
}

// 窓が経過していないバーを落とす(live aggregator は形成中バーを末尾に足すので、それで確定させない)。
func completedBars(cs []market.Candle, now time.Time) []market.Candle {
	out := make([]market.Candle, 0, len(cs))
	for _, c := range cs {
		if c.Interval > 0 && !c.OpenTime.Add(c.Interval).After(now) {
			out = append(out, c)
		}
	}
	return out
}
