package app

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

func rc(sym string, st config.StrategyName, score float64, trig bool) strategy.Candidate {
	return strategy.Candidate{Symbol: sym, Strategy: st, Score: score, Triggered: trig}
}

// ランキングは**戦略ごとにまとめる**。スコアの定義が戦略ごとに違う(abs_momentum は
// 上限なし、donchian は構造上 1.0 付近が上限)ため、全体を score 降順で並べると
// abs_momentum が表示枠を独占する。実測: arm 済み 9 銘柄のうち 7 銘柄が
// 上位15に入らず不可視だった。
func TestGroupRanking_GroupsByStrategyAndKeepsArmed(t *testing.T) {
	ranked := []strategy.Candidate{
		rc("A1", "abs_momentum", 1.76, true), rc("A2", "abs_momentum", 1.67, true),
		rc("A3", "abs_momentum", 1.66, true), rc("A4", "abs_momentum", 1.60, true),
		rc("T1", "atr_breakout", 1.12, true), rc("T2", "atr_breakout", 1.09, true),
		rc("D1", "donchian_breakout", 1.02, true),
	}
	armed := map[string]bool{armKey("A1", "abs_momentum"): true, armKey("T2", "atr_breakout"): true}
	groups := groupRanking(ranked, armed, nil, 2, nil)

	if len(groups) != 3 {
		t.Fatalf("グループ数 = %d, want 3 (戦略ごと)", len(groups))
	}
	// トリガー数の多い戦略が先。
	if groups[0].Strategy != "abs_momentum" || groups[0].Triggered != 4 {
		t.Fatalf("先頭グループ = %+v, want abs_momentum/4", groups[0])
	}
	if groups[1].Strategy != "atr_breakout" || groups[1].Triggered != 2 {
		t.Fatalf("2番目 = %+v, want atr_breakout/2", groups[1])
	}
	if len(groups[0].Rows) != 2 {
		t.Fatalf("abs の表示行 = %d, want 2", len(groups[0].Rows))
	}
	// 全体 score 順なら圏外だった arm 済み T2 が見えること。
	found := false
	for _, r := range groups[1].Rows {
		if r.Symbol == "T2" && r.Armed {
			found = true
		}
	}
	if !found {
		t.Fatalf("arm 済み T2 が atr グループに出ていない: %+v", groups[1].Rows)
	}
}

// topN 圏外の arm 済み銘柄も必ず表示する(発注待ちが画面から消えるのが元の不具合)。
func TestGroupRanking_AlwaysShowsArmedBelowTopN(t *testing.T) {
	ranked := []strategy.Candidate{
		rc("A1", "abs_momentum", 1.9, true), rc("A2", "abs_momentum", 1.8, true),
		rc("A3", "abs_momentum", 1.7, true), rc("A9", "abs_momentum", 1.1, true),
	}
	groups := groupRanking(ranked, map[string]bool{armKey("A9", "abs_momentum"): true}, nil, 2, nil)
	syms := map[string]bool{}
	for _, r := range groups[0].Rows {
		syms[r.Symbol] = true
	}
	if !syms["A9"] {
		t.Fatalf("topN 圏外の arm 済み A9 が消えている: %+v", groups[0].Rows)
	}
	if !syms["A1"] || !syms["A2"] {
		t.Fatalf("上位が落ちている: %+v", groups[0].Rows)
	}
}

// 建玉中の銘柄は held として区別する(発注待ちと混同させない)。
func TestGroupRanking_MarksHeld(t *testing.T) {
	ranked := []strategy.Candidate{rc("A1", "abs_momentum", 1.5, true)}
	groups := groupRanking(ranked, nil, map[string]bool{armKey("A1", "abs_momentum"): true}, 3, nil)
	if !groups[0].Rows[0].Held || groups[0].Rows[0].Armed {
		t.Fatalf("held の区別ができていない: %+v", groups[0].Rows[0])
	}
}

// 未トリガーの戦略で画面を埋めない。
func TestGroupRanking_SkipsUntriggeredStrategies(t *testing.T) {
	ranked := []strategy.Candidate{rc("D1", "donchian_breakout", 0.9, false)}
	if got := groupRanking(ranked, nil, nil, 3, nil); len(got) != 0 {
		t.Fatalf("未トリガー戦略が出ている: %+v", got)
	}
}

// ★ は **arm された戦略のグループにだけ**付ける。1銘柄は複数のスクリーナーで
// トリガーするので、「arm 済みか」だけで付けると実行されない戦略が発注を待っている
// ように見える。実測: 8 銘柄の arm が ★ 18 個に膨らんでいた。
func TestGroupRanking_StarsOnlyTheArmedStrategy(t *testing.T) {
	ranked := []strategy.Candidate{
		rc("6724", "abs_momentum", 1.5, true),
		rc("6724", "atr_breakout", 1.2, true),
		rc("6724", "donchian_breakout", 1.03, true),
	}
	groups := groupRanking(ranked, map[string]bool{armKey("6724", "abs_momentum"): true}, nil, 3, nil)

	stars := 0
	for _, g := range groups {
		for _, r := range g.Rows {
			if r.Armed {
				stars++
				if g.Strategy != "abs_momentum" {
					t.Errorf("arm されていない戦略 %s に ★ が付いた", g.Strategy)
				}
			}
		}
	}
	if stars != 1 {
		t.Fatalf("★ = %d 個, want 1(arm は 1 銘柄 1 戦略)", stars)
	}
	total := 0
	for _, g := range groups {
		total += g.Armed
	}
	if total != 1 {
		t.Fatalf("armed 合計 = %d, want 1", total)
	}
}

// 🛑 スクリーナー(発火判定)は資金を見ない。live は 1単元の建玉金額上限が別に立っていて、
// 超える銘柄は**発火しても arm されない**ので、素の発火数だけを出すと「発注直前の候補」に
// 見える。実測: 発火 2 件(4704 ¥535,200 / 6728 ¥787,000)は上限 350,000 に対して
// 両方とも対象外で、画面は「発火中の候補 2 / 発注待ち 0」のまま一日動かなかった。
// 行に印を付け、グループ側で「そのうち何件が資金上限で対象外か」を数える。
func TestGroupRanking_MarksCandidatesBlockedByNotionalCap(t *testing.T) {
	ranked := []strategy.Candidate{
		rc("6728", "bnf_reversion", 1.24, true), // 787,000 > cap
		rc("4704", "bnf_reversion", 1.40, true), // 535,200 > cap
		rc("2413", "bnf_reversion", 1.10, true), // 上限内
	}
	// 🛑 キーは **(銘柄, 戦略)**(優先ティア導入)。ティアごとに建玉金額
	// 上限が違うので、銘柄だけのキーだと別戦略の行にまで印が漏れる。
	tooBig := map[string]bool{
		UnaffordableKey("6728", config.StrategyBNFReversion): true,
		UnaffordableKey("4704", config.StrategyBNFReversion): true,
	}
	groups := groupRanking(ranked, nil, nil, 5, tooBig)

	if len(groups) != 1 {
		t.Fatalf("グループ数 = %d, want 1", len(groups))
	}
	g := groups[0]
	// 発火数そのものは黙って減らさない(「トリガーが減った」と「対象が狭い」の取り違え防止)。
	if g.Triggered != 3 {
		t.Fatalf("Triggered = %d, want 3(発火数は素のまま)", g.Triggered)
	}
	if g.BlockedNotional != 2 {
		t.Fatalf("BlockedNotional = %d, want 2(6728 / 4704)", g.BlockedNotional)
	}
	byS := map[string]RankingRow{}
	for _, r := range g.Rows {
		byS[r.Symbol] = r
	}
	if !byS["6728"].BlockedNotional || !byS["4704"].BlockedNotional {
		t.Fatalf("上限超の行に印が無い: %+v", g.Rows)
	}
	if byS["2413"].BlockedNotional {
		t.Fatalf("上限内の 2413 に印が付いている: %+v", byS["2413"])
	}
}

// 上限外の集合が未計測(nil)なら誰にも印を付けない — 「上限内だと断定した表示」に
// 倒さず、単に印が無いだけにする。
func TestGroupRanking_NoNotionalMarksWhenUnknown(t *testing.T) {
	groups := groupRanking([]strategy.Candidate{rc("6728", "bnf_reversion", 1.24, true)}, nil, nil, 5, nil)

	if groups[0].BlockedNotional != 0 || groups[0].Rows[0].BlockedNotional {
		t.Fatalf("未計測なのに印が付いた: %+v", groups[0])
	}
}

// 🛑 **売り(空売り)の発火が画面から消えない**ようにする。
//
// `direction: both` の 4 戦略は売りも出すが、グループは
// 戦略名だけで畳んでいたため **買いと売りが 1 つの「発火 N 件」に合算**されていた。
// 実測: donchian は 買い10 + 売り3 = 13 と表示され、売りが 3 件
// 出ていることを画面から知る方法が無かった。
//
// 事前登録は「売りの標本がゼロなら、それは一覧が空だからか・真に非貸借だからか・
// そもそもトリガーが出ていないからか」を切り分けると事前登録している。
// 内訳が出ないと 3 つ目が読めない。
func TestGroupRankingCountsBuyAndSellSeparately(t *testing.T) {
	ranked := []strategy.Candidate{
		{Symbol: "7203", Strategy: config.StrategyDonchianBreakoutV2, Triggered: true, Side: order.SideBuy, Score: 3},
		{Symbol: "6758", Strategy: config.StrategyDonchianBreakoutV2, Triggered: true, Side: order.SideSell, Score: 2},
		{Symbol: "9984", Strategy: config.StrategyDonchianBreakoutV2, Triggered: true, Side: order.SideSell, Score: 1},
		// 発火していない行は数えない(既存の規則)。
		{Symbol: "4751", Strategy: config.StrategyDonchianBreakoutV2, Triggered: false, Side: order.SideSell},
	}
	got := groupRanking(ranked, nil, nil, 3, nil)
	if len(got) != 1 {
		t.Fatalf("groups=%d, want 1", len(got))
	}
	g := got[0]
	if g.Triggered != 3 {
		t.Errorf("Triggered=%d, want 3(合計は従来どおり)", g.Triggered)
	}
	if g.Buy != 1 {
		t.Errorf("Buy=%d, want 1", g.Buy)
	}
	if g.Sell != 2 {
		t.Errorf("Sell=%d, want 2 — 売りの発火が画面から消える", g.Sell)
	}
}

// 買い専用スクリーナー(Side が空)を「売り」に数えない。
func TestGroupRankingTreatsEmptySideAsBuy(t *testing.T) {
	ranked := []strategy.Candidate{
		{Symbol: "7203", Strategy: config.StrategyBNFReversion, Triggered: true, Score: 1},
	}
	got := groupRanking(ranked, nil, nil, 3, nil)
	if len(got) != 1 {
		t.Fatalf("groups=%d, want 1", len(got))
	}
	if got[0].Sell != 0 || got[0].Buy != 1 {
		t.Errorf("Buy=%d Sell=%d, want 1/0 — Side 未設定は買い専用スクリーナー", got[0].Buy, got[0].Sell)
	}
}
