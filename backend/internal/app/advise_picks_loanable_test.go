package app

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

// 🚨 貸借銘柄でない銘柄の**売り候補が枠を占有する**と、同じ戦略の下位にいる買い候補が
// arm されない。ゲートは発注時(trading_cycle.go)にあるので、候補は arm されて
// `per_strategy_n` の枠を1つ潰し、**建玉にならないまま日中ずっと居座る**。
//
// 🛑 これは `loanable_gate.go` が宣言している「買いには一切効かない」という不変条件を
// 実装が破っている形。向きを開ける変更が買い側の測定に
// 触れないためには、**枠を配る前に**落とす必要がある。
func TestSelectAdvisePicks_DropsUnshortableSellCandidates(t *testing.T) {
	ranked := []strategy.Candidate{
		{Symbol: "1111", Strategy: "abs_momentum_v2", Triggered: true, Score: 3.0, Side: order.SideSell},
		{Symbol: "2222", Strategy: "abs_momentum_v2", Triggered: true, Score: 2.0, Side: order.SideSell},
		{Symbol: "3333", Strategy: "abs_momentum_v2", Triggered: true, Score: 1.0, Side: order.SideBuy},
	}
	// 1111 / 2222 は貸借銘柄でない。3333 は買いなので貸借とは無関係。
	shortAllowed := func(sym string) bool { return false }

	got := selectAdvisePicks(ranked, nil, 2, 2, 0, shortAllowed)
	if len(got) != 1 || got[0].Symbol != "3333" {
		t.Fatalf("picks = %+v, want 3333 の買いだけ(売り 2 本が枠を潰していない)", got)
	}
}

// 買い候補は貸借一覧の影響を一切受けない(loanable_gate.go の不変条件)。
func TestSelectAdvisePicks_BuySideUnaffected(t *testing.T) {
	ranked := []strategy.Candidate{
		{Symbol: "1111", Strategy: "atr_breakout_v2", Triggered: true, Score: 3.0, Side: order.SideBuy},
		{Symbol: "2222", Strategy: "atr_breakout_v2", Triggered: true, Score: 2.0, Side: order.SideBuy},
	}
	got := selectAdvisePicks(ranked, nil, 2, 2, 0, func(string) bool { return false })
	if len(got) != 2 {
		t.Fatalf("picks = %d, want 2 — 貸借一覧が買い側を削っている", len(got))
	}
}

// 一覧にある銘柄の売りは通る。
func TestSelectAdvisePicks_KeepsLoanableSells(t *testing.T) {
	ranked := []strategy.Candidate{
		{Symbol: "7203", Strategy: "donchian_breakout_v2", Triggered: true, Score: 3.0, Side: order.SideSell},
	}
	got := selectAdvisePicks(ranked, nil, 2, 2, 0, func(s string) bool { return s == "7203" })
	if len(got) != 1 {
		t.Fatalf("picks = %+v, want 貸借銘柄の売りは通る", got)
	}
}

// nil(=判定器が配線されていない)は**従来どおり全部通す**。ここで fail-close に
// 倒すと、貸借と無関係な buy_only の戦略まで巻き添えで止まる。売りの fail-close は
// 発注前ゲート(risk.EvaluateShortLoanable)が持っている。
func TestSelectAdvisePicks_NilPredicateKeepsEverything(t *testing.T) {
	ranked := []strategy.Candidate{
		{Symbol: "1111", Strategy: "abs_momentum_v2", Triggered: true, Score: 3.0, Side: order.SideSell},
	}
	if got := selectAdvisePicks(ranked, nil, 2, 2, 0, nil); len(got) != 1 {
		t.Fatalf("picks = %+v, want 判定器 nil なら従来どおり", got)
	}
}

// Side が空(買い専用スクリーナー)の候補は売り扱いしない。
func TestSelectAdvisePicks_EmptySideIsNotASell(t *testing.T) {
	ranked := []strategy.Candidate{
		{Symbol: "1111", Strategy: config.StrategyName("bnf_reversion"), Triggered: true, Score: 3.0},
	}
	if got := selectAdvisePicks(ranked, nil, 2, 2, 0, func(string) bool { return false }); len(got) != 1 {
		t.Fatalf("picks = %+v, want Side 空は買い扱い", got)
	}
}

// 🚨 **一覧が未 commit のときは間引かない。**
//
// 枠の前で全 SELL を落とすと、発注前ゲート(risk.EvaluateShortLoanable)まで届かず
// **`loanable_list_missing` が signal_rejections に 1 行も残らなくなる**。それは
// 貸借ゲートが明示的に設計した「売り標本がゼロなのは一覧が無いからだ、と後から分かる」を
// 壊す(監査で「production では一度も記録されない」と指摘されたのと同じ形)。
//
// 一覧が無いのは**起動時の設定漏れ**であって、静かに最適化して隠す状態ではない。
// 枠の公平性(この間引きの目的)は一覧がある前提で意味を持つ。
func TestSelectAdvisePicks_KeepsSellsWhenTheListIsNotConfigured(t *testing.T) {
	ranked := []strategy.Candidate{
		{Symbol: "1111", Strategy: "abs_momentum_v2", Triggered: true, Score: 3.0, Side: order.SideSell},
		{Symbol: "2222", Strategy: "abs_momentum_v2", Triggered: true, Score: 2.0, Side: order.SideBuy},
	}
	// 一覧が空 = 未 commit。判定器そのものを渡さないのが正しい配線。
	hl := &config.HardLimits{AllowedSymbols: []string{"1111", "2222"}}
	got := selectAdvisePicks(ranked, nil, 2, 2, 0, ShortAllowedOrNil(hl))
	if len(got) != 2 {
		t.Fatalf("picks = %+v, want 2 — 一覧未 commit で売りを枠の前で落とすと "+
			"loanable_list_missing が台帳に残らない", got)
	}
}

// 一覧があるときは間引く(枠の公平性)。
func TestSelectAdvisePicks_FiltersWhenTheListIsConfigured(t *testing.T) {
	ranked := []strategy.Candidate{
		{Symbol: "1111", Strategy: "abs_momentum_v2", Triggered: true, Score: 3.0, Side: order.SideSell},
		{Symbol: "2222", Strategy: "abs_momentum_v2", Triggered: true, Score: 2.0, Side: order.SideBuy},
	}
	hl := &config.HardLimits{AllowedSymbols: []string{"1111", "2222"}, LoanableSymbols: []string{"2222"}}
	got := selectAdvisePicks(ranked, nil, 2, 2, 0, ShortAllowedOrNil(hl))
	if len(got) != 1 || got[0].Symbol != "2222" {
		t.Fatalf("picks = %+v, want 2222 の買いだけ", got)
	}
}
