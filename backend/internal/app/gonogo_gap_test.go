package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/strategy"
)

func TestArmedRankedSymbols(t *testing.T) {
	groups := []RankingGroup{
		{Strategy: "bnf_day2_reversion", Rows: []RankingRow{
			{Candidate: strategy.Candidate{Symbol: "7203", Strategy: config.StrategyBNFDay2Reversion}, Armed: true},
			{Candidate: strategy.Candidate{Symbol: "6758", Strategy: config.StrategyBNFDay2Reversion}},
		}},
		{Strategy: "post_jump_drift", Rows: []RankingRow{
			{Candidate: strategy.Candidate{Symbol: "9984", Strategy: config.StrategyPostJumpDrift}, Armed: true},
		}},
	}
	got := ArmedRankedSymbols(groups, func(n config.StrategyName) bool { return n == config.StrategyBNFDay2Reversion })
	if len(got) != 1 || got[0] != "7203" {
		t.Fatalf("got %v", got)
	}
	if all := ArmedRankedSymbols(groups, nil); len(all) != 2 {
		t.Fatalf("nil = 全戦略: %v", all)
	}
}

// 食い違い(arm 済みなのに判定が無い)は集合が変わったときだけログに出す(1 秒ごとの画面更新で埋めない)。
func TestGoNoGoGapLog_LogsOnlyOnChange(t *testing.T) {
	var buf bytes.Buffer
	g := &GoNoGoGapLog{Logger: slog.New(slog.NewTextHandler(&buf, nil)), Track: "live"}
	g.Observe([]string{"7203"})
	g.Observe([]string{"7203"})
	g.Observe([]string{})
	if n := strings.Count(buf.String(), "gonogo_missing_for_armed"); n != 2 {
		t.Fatalf("変化のたびに 1 回: %d 回\n%s", n, buf.String())
	}
}

// 画面の「未判定を判定」ボタンが判定する集合 = research と live の arm 済みで判定が無い銘柄の和。
// 各トラックの画面が更新されるたびに、そのトラックの分だけを置き換える。
func TestGoNoGoMissing_UnionAcrossTracks(t *testing.T) {
	m := &GoNoGoMissing{}
	research := &GoNoGoGapLog{Track: "research", Missing: m}
	live := &GoNoGoGapLog{Track: "live", Missing: m}
	research.Observe([]string{"7203", "6594"})
	live.Observe([]string{"6594", "9984"})
	if got := strings.Join(m.All(), ","); got != "6594,7203,9984" {
		t.Fatalf("和 = %s", got)
	}
	research.Observe([]string{}) // research は判定が揃った
	if got := strings.Join(m.All(), ","); got != "6594,9984" {
		t.Fatalf("置き換え後 = %s", got)
	}
}
