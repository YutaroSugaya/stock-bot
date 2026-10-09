package strategy

import (
	"fmt"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/ta"
)

// v2 戦略。1戦略につき変更は入口の1点だけで、
// 出口は v1 と同一(enterIfClears の TP=3.0×ATR / SL=2.0×ATR)。複数変えると差分を入口に帰属できない。
// **変更の根拠は損益ではなく構造**: 9営業日の損益を見て負けた戦略の入口を直すのは事後フィッティング
// (EDGE_METHODOLOGY が禁止)なので、v2 を作ったのは損益を見ずに指摘できる欠陥がある 3 戦略だけ。
// high_volume_premium / post_jump_drift / high_52w_momentum は欠陥を指摘できないので v1 のまま。

// v1 との唯一の差: 条件が偽→真に転じたバーだけ入る(状態ではなく事象として扱う)。
type AbsMomentumV2 struct{}

func (AbsMomentumV2) Name() config.StrategyName { return config.StrategyAbsMomentumV2 }

// v1 の入口式を1箇所に置く(v2 は今バーと前バーで同じ式を2回使う。分岐させると「入口の1点だけが違う」が崩れる)。
func absMomentumSideAt(d []market.Candle) order.Side {
	if len(d) < absTrendSMA+1 {
		return ""
	}
	cl := closesOf(d)
	sma := ta.SMA(cl, absTrendSMA)
	if sma <= 0 || len(cl) < absLookback+2 {
		return ""
	}
	last := cl[len(cl)-1]
	past := cl[len(cl)-1-absLookback]
	switch {
	case last > sma && last > past:
		return order.SideBuy
	case last < sma && last < past:
		return order.SideSell
	default:
		return ""
	}
}

func (s AbsMomentumV2) Evaluate(in EvalInput) Signal {
	name := s.Name()
	// v1 と同じ履歴要件 + 前バーでも判定するので 1 本余分に要る。
	if sig, ok := evalGuard(in, name, absTrendSMA+2); !ok {
		return sig
	}
	d := in.CandlesDaily
	side := absMomentumSideAt(d)
	if side == "" {
		return noTradeSignal(in, name, "no_abs_momentum")
	}
	// ★ v1 との唯一の差: 前バーで既に同じ向きが成立していたら見送る。
	if absMomentumSideAt(d[:len(d)-1]) == side {
		return noTradeSignal(in, name, "not_fresh_cross")
	}
	return enterIfClears(in, side, name, "abs_momentum_fresh_cross", dcSlippageTicks, dcMinEdgeMultiple)
}

// score の定義は v1 のまま(変えると v1 と分位が比較不能になる)。
func (AbsMomentumV2) Screen(symbol string, d []market.Candle) Candidate {
	c := AbsMomentum{}.Screen(symbol, d)
	c.Strategy = config.StrategyAbsMomentumV2
	if !c.Triggered {
		return c
	}
	fresh := absMomentumSideAt(d) != "" && absMomentumSideAt(d[:len(d)-1]) != absMomentumSideAt(d)
	c.Triggered = fresh
	c.Conditions = append(c.Conditions, Condition{
		Label: "本日が成立初日(状態継続では入らない)", Met: fresh,
		Got: map[bool]string{true: "初日", false: "継続中"}[fresh],
	})
	return c
}

// 圧縮は閾値ではなく ATR 2本の大小で書く(過去の損益に当てた値を1つも増やさないため)。
const atrCompressionSlow = 50

// v1 との唯一の差: 直前まで短期ボラが収縮していた拡大だけを取る。
type ATRBreakoutV2 struct{}

func (ATRBreakoutV2) Name() config.StrategyName { return config.StrategyATRBreakoutV2 }

// 判定はブレイク前日時点で行う(当日の値幅を含めると、拡大自体が ATR(14) を押し上げて自己言及になる)。
func priorCompression(d []market.Candle) (fast, slow float64, ok bool) {
	if len(d) < atrCompressionSlow+2 {
		return 0, 0, false
	}
	prior := d[:len(d)-1]
	fast = ta.ATR(prior, atrN)
	slow = ta.ATR(prior, atrCompressionSlow)
	if fast <= 0 || slow <= 0 {
		return fast, slow, false
	}
	return fast, slow, fast < slow
}

// 入口の判定を 1 箇所に置く(兄弟アームと **screener** が同じ関数を呼ぶため。
// 分岐させると「入口が完全に同一」が崩れ、差を出口に帰属できなくなる /
// screener と入口がズレると「枠は取れるのに永久に建たないアーム」ができる)。
// side=="" のとき理由を返す。
//
// v1 相当(レンジ拡大のみ・圧縮を見ない)。v1 の screener はこちらを使う。
func atrRangeExpansionSideAt(d []market.Candle) (order.Side, string) {
	if len(d) < 2 {
		return "", "insufficient_history"
	}
	cl := closesOf(d)
	atr := ta.ATR(d, atrN)
	if atr <= 0 {
		return "", "no_atr"
	}
	sma := ta.SMA(cl, atrTrendSMA)
	if sma <= 0 {
		return "", "no_sma"
	}
	prevClose := d[len(d)-2].Close
	last := d[len(d)-1].Close
	switch {
	case last > prevClose+atr && last > sma:
		return order.SideBuy, ""
	case last < prevClose-atr && last < sma:
		return order.SideSell, ""
	}
	return "", "no_range_expansion"
}

// v2 = v1 + 「直前まで短期ボラが収縮していた拡大だけを取る」。
func atrBreakoutSideAt(d []market.Candle) (order.Side, string) {
	side, reason := atrRangeExpansionSideAt(d)
	if side == "" {
		return "", reason
	}
	// ★ v1 との唯一の差。
	if _, _, compressed := priorCompression(d); !compressed {
		return "", "no_prior_compression"
	}
	return side, ""
}

func (s ATRBreakoutV2) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, atrTrendSMA+1); !ok {
		return sig
	}
	side, reason := atrBreakoutSideAt(in.CandlesDaily)
	if side == "" {
		return noTradeSignal(in, name, reason)
	}
	return enterIfClears(in, side, name, "atr_breakout_from_compression", dcSlippageTicks, dcMinEdgeMultiple)
}

func (ATRBreakoutV2) Screen(symbol string, d []market.Candle) Candidate {
	c := ATRBreakout{}.Screen(symbol, d)
	c.Strategy = config.StrategyATRBreakoutV2
	if !c.Triggered {
		return c
	}
	fast, slow, compressed := priorCompression(d)
	c.Triggered = compressed
	got := "履歴不足"
	if fast > 0 && slow > 0 {
		got = fmt.Sprintf("ATR14 %.1f vs ATR50 %.1f", fast, slow)
	}
	c.Conditions = append(c.Conditions, Condition{
		Label: "直前まで値幅が収縮(ATR14 < ATR50)", Met: compressed, Got: got,
	})
	return c
}

// v1 との唯一の差: ブレイク初日だけを取る(前バーが既にチャネル外なら見送り)。
type DonchianBreakoutV2 struct{}

func (DonchianBreakoutV2) Name() config.StrategyName { return config.StrategyDonchianBreakoutV2 }

func donchianSideAt(d []market.Candle) order.Side {
	if len(d) < dbWindow+1 {
		return ""
	}
	hi, lo, ok := ta.Donchian(d[:len(d)-1], dbWindow)
	if !ok {
		return ""
	}
	last := d[len(d)-1].Close
	switch {
	case last >= hi:
		return order.SideBuy
	case last <= lo:
		return order.SideSell
	default:
		return ""
	}
}

func (s DonchianBreakoutV2) Evaluate(in EvalInput) Signal {
	name := s.Name()
	// v1 と同じ履歴要件 + 前バーでも判定するので 1 本余分に要る。
	if sig, ok := evalGuard(in, name, dbWindow+2); !ok {
		return sig
	}
	d := in.CandlesDaily
	side := donchianSideAt(d)
	if side == "" {
		return noTradeSignal(in, name, "no_breakout")
	}
	// ★ v1 との唯一の差: 前バーで既に同じ向きに抜けていたら見送る(初日のみ)。
	if donchianSideAt(d[:len(d)-1]) == side {
		return noTradeSignal(in, name, "not_first_breakout")
	}
	return enterIfClears(in, side, name, "donchian_first_breakout", dcSlippageTicks, dcMinEdgeMultiple)
}

func (DonchianBreakoutV2) Screen(symbol string, d []market.Candle) Candidate {
	c := DonchianBreakout{}.Screen(symbol, d)
	c.Strategy = config.StrategyDonchianBreakoutV2
	if !c.Triggered {
		return c
	}
	side := donchianSideAt(d)
	first := side != "" && donchianSideAt(d[:len(d)-1]) != side
	c.Triggered = first
	c.Conditions = append(c.Conditions, Condition{
		Label: "本日がブレイク初日(継続では入らない)", Met: first,
		Got: map[bool]string{true: "初日", false: "継続中"}[first],
	})
	return c
}
