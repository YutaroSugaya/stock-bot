package app

import (
	"fmt"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/strategy"
)

// arm しうる候補を **arm と同じ関数**で出す(寄り前の go/no-go の判定対象・cmd/gonogo)。
//
// 🛑 ランキングを再実装しない。arm と食い違うと、arm された銘柄が「未判定」になる。
// ここに無いのは bot の状態(建玉・口座の資金枠・人間の停止)だけで、それを見ない分だけ
// 候補は arm 済みの集合より広く取る(呼び手が入口ごとの枠を広げて渡す)。

// PaperArmCandidates は paper(advisor 経路)が arm しうる候補。universe の 1 単元の金額上限は
// AdvisorLoop.universe と、枠の配り方は selectAdvisePicks と同じ。建玉は見ない(isHeld = nil)ので、
// 建玉で飛ばされるぶんは perStrategy を広げて吸収する。全体の上限は掛けない(候補を削らない)。
func PaperArmCandidates(universe map[string][]market.Candle, screeners []strategy.Screener,
	maxNotionalJPY, perStrategy int, shortAllowed func(string) bool) []strategy.Candidate {
	u := make(map[string][]market.Candle, len(universe))
	for sym, cs := range universe {
		if lotAffordable(maxNotionalJPY, cs) {
			u[sym] = cs
		}
	}
	ranked := strategy.RankCandidates(u, screeners)
	return selectAdvisePicks(ranked, nil, len(ranked)+1, perStrategy, 0, shortAllowed)
}

// LiveArmCandidates は live の selector がそのティアで arm しうる**発火済み**の候補を、Tick と同じ順と
// 同じ篩(arm config・1 単元の金額・計画損失)で上位 n 本まで返す。universe は ScreenableDaily を
// 通したもの。口座の状態(建玉・資金枠・停止)は見ない。
//
// 発火していない候補を落とすのは、場中に建ちうるのが発火済みだけだから(bnf 家族の Screen の
// Triggered は前日の確定足の条件)。selector は最下位ティアで未発火も arm するが、それは枠を
// 遊ばせないためで、その日には建たない。
func LiveArmCandidates(universe map[string][]market.Candle, tier SelectorTier, maxRiskPerTradeJPY, n int) []strategy.Candidate {
	var out []strategy.Candidate
	for _, c := range strategy.RankCandidates(universe, tier.Screeners) {
		if len(out) >= n {
			break
		}
		if !c.Triggered {
			break // 並びは発火済みが先頭
		}
		if tier.ArmCfg == nil {
			continue
		}
		if !tier.admits(tier.ArmCfg(c.Symbol), universe[c.Symbol], c, maxRiskPerTradeJPY) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// admits は口座の状態に依らない arm の篩(Tick と LiveArmCandidates が共有する)。
func (t SelectorTier) admits(cfg *config.StrategyConfig, daily []market.Candle, c strategy.Candidate, maxRiskPerTradeJPY int) bool {
	reason, _ := t.rejectReason(cfg, daily, c, maxRiskPerTradeJPY)
	return reason == ""
}

// rejectReason は admits の理由つき版。空なら通す。理由は signal_rejections の安定種別
// (発注前ゲートと同じ語を使い、selector 側は `selector_` を前置して書く)、detail は可変部。
//
// 🚨 計画損失が上限を超えて落ちた候補が、どこにも残らない形があった。
func (t SelectorTier) rejectReason(cfg *config.StrategyConfig, daily []market.Candle, c strategy.Candidate, maxRiskPerTradeJPY int) (reason, detail string) {
	if cfg == nil {
		return "arm_cfg_nil", ""
	}
	if !affordableIn(t.MaxNotionalJPY, cfg, daily) {
		return "notional_over_cap", fmt.Sprintf("1 単元 %d > 上限 %d", lotNotionalJPY(cfg, daily), t.MaxNotionalJPY)
	}
	if !withinRiskPerTrade(maxRiskPerTradeJPY, cfg, c) {
		return "risk_per_trade", fmt.Sprintf("計画損失 %d > 上限 %d", int(c.StopLossJPY*float64(cfg.Risk.Quantity)), maxRiskPerTradeJPY)
	}
	return "", ""
}

// lotNotionalJPY は 1 単元の金額(直近終値 × 株数)。日足が無ければ 0。
func lotNotionalJPY(cfg *config.StrategyConfig, daily []market.Candle) int {
	if cfg == nil || len(daily) == 0 {
		return 0
	}
	return int(daily[len(daily)-1].Close * float64(cfg.Risk.Quantity))
}

// ScreenableDaily はその日足をランキングに入れてよいか。理由が空なら可(selector と go/no-go が共有)。
// 🛑 分割未調整の系列は入れない(25日線が実勢の数倍に居座った偽のパニックが逆張りの首位に立つ)。
func ScreenableDaily(cs []market.Candle) (ok bool, reason string) {
	if len(cs) < 26 {
		return false, "insufficient_history"
	}
	if d := market.SplitDiscontinuity(cs); d != "" {
		return false, d
	}
	return true, ""
}

// SelectorCandleLookback はランキングに供給する日足の本数(selector と go/no-go が共有)。
const SelectorCandleLookback = selectorCandleLookback
