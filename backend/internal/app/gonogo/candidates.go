package gonogo

import (
	"fmt"

	"stockbot/backend/internal/app"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/strategy"
)

// PaperFamily は判定する paper の入口(bnf 家族)。`_trail` は入口が同じなので同じ候補になる。
var PaperFamily = []config.StrategyName{
	config.StrategyBNFReversion,
	config.StrategyBNFDay2Reversion,
	config.StrategyBNFStabilizedReversion,
	config.StrategyBNFIntradayReversion,
}

// PaperHeldMargin は入口あたりの余裕。go/no-go は建玉を見ないので、建玉で飛ばされて下位が
// 繰り上がるぶん(同じ日に建った銘柄・前日までの多日建玉)を per_strategy_n に足して取る。
const PaperHeldMargin = 3

// Inputs は候補を出す材料。Paper / Live が nil ならそのトラックは判定しない。
type Inputs struct {
	// Universe は今日のユニバースの日足(app.ScreenableDaily を通したもの)。
	Universe      map[string][]market.Candle
	Paper         *config.BotConfig
	Live          *config.BotConfig
	LiveTemplates []*config.StrategyConfig // 優先順(先頭が最優先)
	HardLimits    *config.HardLimits
}

// Candidates は paper の bnf 家族と live が今日 arm しうる候補を、arm と同じ関数で出す。
func Candidates(in Inputs) (paper, live []strategy.Candidate, err error) {
	if in.Paper != nil {
		var entries []config.StrategyName
		for _, e := range in.Paper.Advisor.Entries {
			for _, f := range PaperFamily {
				if e == f {
					entries = append(entries, e)
				}
			}
		}
		if len(entries) > 0 {
			sc, err := strategy.ScreenersForEntries(entries)
			if err != nil {
				return nil, nil, fmt.Errorf("paper の入口: %w", err)
			}
			perEntry := in.Paper.Advisor.PerStrategyN
			if perEntry <= 0 {
				perEntry = 1
			}
			paper = app.PaperArmCandidates(in.Universe, sc, in.Paper.Selector.MaxPositionNotionalJPY,
				perEntry+PaperHeldMargin, app.ShortAllowedOrNil(in.HardLimits))
		}
	}
	if in.Live != nil {
		seen := map[string]bool{}
		for _, tmpl := range in.LiveTemplates {
			if tmpl == nil || in.HardLimits == nil || !in.HardLimits.AllowsLiveStrategy(tmpl.StrategyName) {
				continue // allowlist の外は bot も起動しない
			}
			tmpl := tmpl
			tier := app.SelectorTier{
				Screeners: strategy.LiveArmScreeners(tmpl.StrategyName),
				ArmCfg: func(sym string) *config.StrategyConfig {
					c := tmpl.ForSymbol(sym)
					if c == nil || c.ValidateAgainstHardLimits(in.HardLimits) != nil {
						return nil
					}
					return c
				},
				MaxNotionalJPY: in.Live.Selector.NotionalCapFor(tmpl.StrategyName),
			}
			for _, c := range app.LiveArmCandidates(in.Universe, tier, in.Live.Risk.MaxRiskPerTradeJPY,
				in.Live.Risk.AccountMaxOpenPositions) {
				if !seen[c.Symbol] {
					seen[c.Symbol] = true
					live = append(live, c)
				}
			}
		}
	}
	return paper, live, nil
}
