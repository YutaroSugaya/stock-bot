package main

import (
	"fmt"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/strategy"
)

// researchScreeners は research(advisor 経路と dashboard のランキング)に渡す screener。
// `advisor_v2.entries` が空ならコードのメニュー全部、あればその入口(+ 兄弟)だけ。
// 未知の名前は起動を止める(fail-close)。
//
// 🛑 live selector の `strategy.LiveArmScreeners`(戦略カタログ)はこの経路を通らない —
// live は `STOCKBOT_LIVE_STRATEGY_CONFIG` と hard_limits の allowlist が別に縛る。
func researchScreeners(botCfg *config.BotConfig) ([]strategy.Screener, error) {
	ss, err := strategy.ScreenersForEntries(botCfg.Advisor.Entries)
	if err != nil {
		return nil, fmt.Errorf("bot_config advisor_v2.entries: %w", err)
	}
	return ss, nil
}
