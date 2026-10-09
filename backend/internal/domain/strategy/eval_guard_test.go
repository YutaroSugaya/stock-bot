package strategy

import (
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
)

// evalGuard は 15 の Evaluate が共有する入口ガード。**1 か所を直すと全戦略の挙動が
// 同時に変わる**ので、契約をここで固定する。
//
// 抽出前は同じ 7 行が 15 箇所に展開されていて、変異テストで次の 2 つが
// **どのテストにも捕まらない**ことが判った(抽出でその穴が 1 か所に集まった):
//   - 理由文字列を変えても緑のまま
//   - config / summary の nil ガードを外しても緑のまま
//
// 理由文字列は signal_rejections の GROUP BY キー(migration 0009)= 「なぜ今日
// エントリーしなかったのか」の監査証跡そのもので、黙って変わると台帳の集計が壊れる。
// nil ガードは config 凍結前の評価を止める入口。どちらも落とせない。
func TestEvalGuard(t *testing.T) {
	bars := func(n int) []market.Candle {
		cs := make([]market.Candle, n)
		for i := range cs {
			cs[i] = market.Candle{Close: 1000}
		}
		return cs
	}
	full := func() EvalInput {
		return EvalInput{
			Now:          time.Unix(0, 0),
			Config:       &config.StrategyConfig{ConfigID: "cfg-1"},
			Summary:      &market.MarketSummary{Symbol: "7203"},
			CandlesDaily: bars(10),
		}
	}

	cases := []struct {
		name       string
		mutate     func(*EvalInput)
		minBars    int
		wantOK     bool
		wantReason string
	}{
		{"config が nil", func(in *EvalInput) { in.Config = nil }, 1, false, "no_config_or_summary"},
		{"summary が nil", func(in *EvalInput) { in.Summary = nil }, 1, false, "no_config_or_summary"},
		{"両方 nil", func(in *EvalInput) { in.Config, in.Summary = nil, nil }, 1, false, "no_config_or_summary"},
		{"日足が minBars 未満", nil, 11, false, "insufficient_daily_history"},
		{"日足が丁度 minBars", nil, 10, true, ""},
		{"日足が minBars 超", nil, 9, true, ""},
		{"日足ゼロ本", func(in *EvalInput) { in.CandlesDaily = nil }, 1, false, "insufficient_daily_history"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := full()
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			sig, ok := evalGuard(in, config.StrategyBNFReversion, tc.minBars)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK {
				return
			}
			if sig.Decision != DecisionNoTrade {
				t.Fatalf("Decision = %q, want %q", sig.Decision, DecisionNoTrade)
			}
			if sig.Reason != tc.wantReason {
				t.Fatalf("Reason = %q, want %q (signal_rejections の集計キー — 黙って変えない)", sig.Reason, tc.wantReason)
			}
			if sig.StrategyName != config.StrategyBNFReversion {
				t.Fatalf("StrategyName = %q, want %q (どの戦略が見送ったか判らないと台帳が読めない)", sig.StrategyName, config.StrategyBNFReversion)
			}
		})
	}
}

// nil ガードが先。config が nil のまま日足だけ足りない入力で
// insufficient_daily_history を返すと、原因の切り分けができない。
func TestEvalGuardChecksNilBeforeHistory(t *testing.T) {
	in := EvalInput{Now: time.Unix(0, 0)} // config / summary / 日足すべて欠落
	sig, ok := evalGuard(in, config.StrategyBNFReversion, 5)
	if ok {
		t.Fatal("全欠落を通してはいけない")
	}
	if sig.Reason != "no_config_or_summary" {
		t.Fatalf("Reason = %q, want no_config_or_summary (nil を履歴不足と報告しない)", sig.Reason)
	}
}
