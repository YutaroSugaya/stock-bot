package app

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/strategy"
)

func cand(sym string, st config.StrategyName, score float64) strategy.Candidate {
	return strategy.Candidate{Symbol: sym, Strategy: st, Score: score, Triggered: true}
}

// スコアは戦略ごとに定義が違うので**比較できない**。abs_momentum は
// min(現在値/200日線, 現在値/6ヶ月前) で上限が無く強気相場では 1.3〜1.8 まで伸びる
// のに対し、donchian は 現在値/20日高値 で「今日の値段が高値に含まれる」ため構造上
// 1.0 付近が上限。全体を score 降順で上から取ると abs_momentum が全枠を独占し、
// 他戦略の標本が永久にゼロになる(実測: LLM の選択 80 件中 73 件が
// abs_momentum、ドンチャンは 19 銘柄トリガーしていたのに 0 件)。
func TestSelectAdvisePicks_RoundRobinsAcrossStrategies(t *testing.T) {
	ranked := []strategy.Candidate{
		cand("A1", "abs_momentum", 1.76), cand("A2", "abs_momentum", 1.67),
		cand("A3", "abs_momentum", 1.66), cand("A4", "abs_momentum", 1.59),
		cand("D1", "donchian_breakout", 1.02), cand("D2", "donchian_breakout", 1.01),
		cand("T1", "atr_breakout", 1.08),
	}
	got := selectAdvisePicks(ranked, nil, 10, 2, 0, nil)

	bySt := map[config.StrategyName]int{}
	for _, c := range got {
		bySt[c.Strategy]++
	}
	if bySt["abs_momentum"] != 2 || bySt["donchian_breakout"] != 2 || bySt["atr_breakout"] != 1 {
		t.Fatalf("戦略ごとの割当 = %+v, want abs2/don2/atr1", bySt)
	}
	// 各戦略の第1候補が、どの戦略の第2候補よりも先に来る(ラウンドロビン)。
	first := map[config.StrategyName]bool{}
	for i, c := range got {
		if i < 3 && first[c.Strategy] {
			t.Fatalf("先頭3枠に同じ戦略が2回来ている: %+v", got)
		}
		first[c.Strategy] = true
	}
	// 各戦略の中ではスコア降順。
	if got[0].Symbol != "A1" {
		t.Errorf("abs_momentum の第1候補が最高スコアでない: %s", got[0].Symbol)
	}
}

// 全体上限(top_n)は残す — LLM の実行時間/コストの安全弁。
func TestSelectAdvisePicks_RespectsTotalCap(t *testing.T) {
	var ranked []strategy.Candidate
	for _, st := range []config.StrategyName{"abs_momentum", "donchian_breakout", "atr_breakout"} {
		for i := 0; i < 5; i++ {
			ranked = append(ranked, cand(string(st)[:3]+string(rune('0'+i)), st, 1.5-float64(i)*0.01))
		}
	}
	if got := selectAdvisePicks(ranked, nil, 4, 3, 0, nil); len(got) != 4 {
		t.Fatalf("len = %d, want 4 (total cap)", len(got))
	}
}

// 建玉中の銘柄は枠を消費しない(その銘柄は凍結 config で走っているので再 arm しない)。
func TestSelectAdvisePicks_SkipsHeld(t *testing.T) {
	ranked := []strategy.Candidate{
		cand("A1", "abs_momentum", 1.7), cand("A2", "abs_momentum", 1.6),
		cand("D1", "donchian_breakout", 1.02),
	}
	held := func(s string, _ config.StrategyName) bool { return s == "A1" }
	got := selectAdvisePicks(ranked, held, 10, 1, 0, nil)
	if len(got) != 2 || got[0].Symbol != "A2" {
		t.Fatalf("got %+v, want A2 と D1 (A1 は建玉中)", got)
	}
}

// **契約が反転した**。以前ここは「同じ銘柄が複数戦略でトリガー
// しても pick は1回」を固定していた(理由: LLM 呼び出しを1回に抑える)。LLM が
// arm 経路から外れ、建玉の一意性キーが (銘柄, 戦略) になったので、**同一銘柄が
// 別戦略で複数 pick されるのが正しい** — 潰していたせいで、入口が同一で出口だけ違う
// 2 アームのペア標本が 1 本も成立していなかった。
func TestSelectAdvisePicks_SameSymbolAcrossStrategies(t *testing.T) {
	ranked := []strategy.Candidate{
		cand("X", "abs_momentum", 1.7), cand("X", "donchian_breakout", 1.02),
		cand("D1", "donchian_breakout", 1.01),
	}
	got := selectAdvisePicks(ranked, nil, 10, 2, 0, nil)
	seen := map[string]int{}
	for _, c := range got {
		seen[c.Symbol]++
	}
	if seen["X"] != 2 {
		t.Fatalf("同一銘柄の別戦略が %d 回しか選ばれていない: %+v", seen["X"], got)
	}
}

// トリガーゼロなら空 — 呼び出し側が「なぜ見送りか」を記録する枝に落とす。
func TestSelectAdvisePicks_EmptyWhenNoTrigger(t *testing.T) {
	ranked := []strategy.Candidate{{Symbol: "A", Strategy: "abs_momentum", Score: 0.8}}
	if got := selectAdvisePicks(ranked, nil, 10, 2, 0, nil); len(got) != 0 {
		t.Fatalf("got %+v, want 空", got)
	}
}

// 入口が同じ2アーム(bnf_reversion と bnf_reversion_trail)が枠を取り合うとき、
// 巡回順を名前昇順で固定すると **常に同じアームが先**になる。全体上限(top_n)が
// 拘束する日には、後ろのアームが毎日こぼれて標本が永久にゼロになる。日ごとに先頭を
// 回して、長期的には均等に配る。(乱数は使わない — 同じ日・同じ入力なら必ず同じ結果)
//
// **同一銘柄の 2 アームは両方 pick される**(以前は 1 回に畳んでいた)。
// 回転が効くのは「どちらが先か」であって「どちらか一方」ではなくなったので、
// 全体上限を 1 に絞って先頭争いだけを見る。
func TestSelectAdvisePicks_RotatesFirstPickAcrossDays(t *testing.T) {
	ranked := []strategy.Candidate{
		cand("X", "bnf_reversion", 1.5),
		cand("X", "bnf_reversion_trail", 1.5), // 同じ銘柄・同じ入口
	}
	// 上限を外せば両アームが揃うことを先に固定する。
	if both := selectAdvisePicks(ranked, nil, 10, 1, 0, nil); len(both) != 2 {
		t.Fatalf("2 アームが揃わない: %+v", both)
	}
	winners := map[config.StrategyName]int{}
	for day := 0; day < 8; day++ {
		got := selectAdvisePicks(ranked, nil, 1, 1, day, nil) // 全体上限 1 = 先頭争い
		if len(got) != 1 {
			t.Fatalf("day %d: len = %d, want 1 (top_n=1)", day, len(got))
		}
		winners[got[0].Strategy]++
	}
	if winners["bnf_reversion"] == 0 || winners["bnf_reversion_trail"] == 0 {
		t.Fatalf("片方のアームが一度も選ばれていない: %+v", winners)
	}
	// 同じ日なら必ず同じ結果(再現性)。
	a := selectAdvisePicks(ranked, nil, 1, 1, 3, nil)
	b := selectAdvisePicks(ranked, nil, 10, 1, 3, nil)
	if a[0].Strategy != b[0].Strategy {
		t.Fatal("同じ日で結果が揺れている(決定論でない)")
	}
}
