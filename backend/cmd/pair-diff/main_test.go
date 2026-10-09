package main

import (
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository/pg"
	"stockbot/backend/internal/backtest/pairdiff"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/usecase/command"
)

// basePairs は**メニューに実在する兄弟アームを 1 組も落とさず**、かつ存在しない
// 名前を持たないこと。ここがずれると、存在しないアーム名で突き合わせて
// 「ペア 0 組」と報告する(静かに壊れる形なので機械で固定する)。
//
// 🛑 メニューから導く。以前は「4 ペア」という数字を焼いていたので、`bnf_reversion` /
// `bnf_reversion_trail`(ペア比較の効果を最初に確かめられる 5 組目)が**入っていない
// ことに誰も気づけなかった**。
func TestBasePairsCoverEveryTrailArmInTheMenu(t *testing.T) {
	inMenu := map[string]bool{}
	for _, n := range command.AdvisorCandidateStrategies {
		inMenu[string(n)] = true
	}
	want := map[string]bool{}
	for n := range inMenu {
		base := string(strategy.EntryArmOf(config.StrategyName(n)))
		if base != n && inMenu[base] {
			want[base] = true
		}
	}
	got := map[string]bool{}
	for _, b := range basePairs {
		got[b] = true
	}
	for b := range want {
		if !got[b] {
			t.Errorf("メニューに %s / %s_trail があるのに basePairs に無い — そのペアは永久に採点されない",
				b, b)
		}
	}
	for b := range got {
		trail := b + strategy.TrailArmSuffix
		if !inMenu[b] || !inMenu[trail] {
			t.Errorf("basePairs の %s はメニューに無い(%s も含めて)— 名前がずれている", b, trail)
		}
	}
}

// 兄弟は**入口が完全に同一**であることの機械的な裏取り: 営業日 MaxHold を持つ
// 戦略なら、基と兄弟でその値が一致していること(片方だけ直す事故を弾く)。
func TestTrailArmsShareTheEntryGeometry(t *testing.T) {
	for _, base := range basePairs {
		trail := base + strategy.TrailArmSuffix
		if strategy.MaxHoldBusinessDays(config.StrategyName(base)) !=
			strategy.MaxHoldBusinessDays(config.StrategyName(trail)) {
			t.Errorf("%s と %s の MaxHold が違う — 兄弟ではない組を見ている", base, trail)
		}
	}
}

func row(sym, strat string, entry float64, qty int, net float64, openedDay string) pg.PairTradeRow {
	d, _ := time.ParseInLocation("2006-01-02", openedDay, clock.JST)
	return pg.PairTradeRow{
		Symbol: sym, Strategy: strat, EntryPrice: entry, Quantity: qty,
		NetJPY: net, OpenedAt: d, ClosedAt: d.Add(48 * time.Hour),
	}
}

func toTrades(rows []pg.PairTradeRow) []pairdiff.Trade {
	out := make([]pairdiff.Trade, 0, len(rows))
	for _, r := range rows {
		out = append(out, pairdiff.Trade{
			Symbol: r.Symbol, Strategy: r.Strategy, EntryPrice: r.EntryPrice,
			NetJPY: r.NetJPY, Day: r.OpenedAt.In(clock.JST).Format("2006-01-02"),
		})
	}
	return out
}

// 🛑 **day_blocks が足りないうちは判定を読ませない**。決済が薄いうちは、
// 数組そろった時点で「トレールが勝った」と読むのがいちばんありそうな誤り。
func TestVerdictRefusesToDecideBelowMinDayBlocks(t *testing.T) {
	rows := []pg.PairTradeRow{
		row("7203", "abs_momentum_v2", 2500, 100, 1000, "2026-08-24"),
		row("7203", "abs_momentum_v2_trail", 2500, 100, 5000, "2026-08-24"),
		row("6758", "abs_momentum_v2", 1200, 100, -300, "2026-08-25"),
		row("6758", "abs_momentum_v2_trail", 1200, 100, 900, "2026-08-25"),
	}
	rep := buildReport("abs_momentum_v2", rows, toTrades(rows), nil, 2000, 42)

	if rep.PairsN != 2 {
		t.Fatalf("ペア = %d, want 2", rep.PairsN)
	}
	if rep.Verdict != "insufficient_day_blocks" {
		t.Fatalf("判定 = %q, want insufficient_day_blocks(day_blocks=%d)", rep.Verdict, rep.DayBlocks)
	}
	if len(rep.Reasons) == 0 || !strings.Contains(rep.Reasons[0], "実効標本は日数") {
		t.Errorf("理由に「実効標本は日数」が無い: %+v", rep.Reasons)
	}
	// trail が両方勝っているが、それでも verdict は trail_better にならない。
	if rep.MeanDiffPer1M <= 0 {
		t.Errorf("平均ペア差が正でない(前提が崩れている): %v", rep.MeanDiffPer1M)
	}
}

// ¥1M 正規化がペア差に効いていること。値がさ株の 1 件が全体を決めないため。
func TestMeanDiffIsNormalisedPer1M(t *testing.T) {
	rows := []pg.PairTradeRow{
		// 25万円の建玉で差 +1,000 → ¥1M あたり +4,000
		row("7203", "atr_breakout_v2", 2500, 100, 0, "2026-08-24"),
		row("7203", "atr_breakout_v2_trail", 2500, 100, 1000, "2026-08-24"),
	}
	rep := buildReport("atr_breakout_v2", rows, toTrades(rows), nil, 100, 42)
	if rep.MeanDiffJPY != 1000 {
		t.Fatalf("素の平均差 = %v, want 1000", rep.MeanDiffJPY)
	}
	if rep.MeanDiffPer1M != 4000 {
		t.Fatalf("¥1M 正規化 = %v, want 4000", rep.MeanDiffPer1M)
	}
}

// 🚨 片側しか建たなかった件は**必ず出力に残る**(コスト床の検出器)。
func TestBrokenPairsSurfaceInTheReport(t *testing.T) {
	rows := []pg.PairTradeRow{
		row("9984", "donchian_breakout_v2", 8000, 100, 500, "2026-08-24"),
	}
	rep := buildReport("donchian_breakout_v2", rows, toTrades(rows), nil, 100, 42)
	if rep.BrokenN != 1 {
		t.Fatalf("片側のみ = %d, want 1", rep.BrokenN)
	}
	if rep.BrokenByArm["donchian_breakout_v2_trail"] != 1 {
		t.Errorf("欠けたアームが特定できていない: %+v", rep.BrokenByArm)
	}
	if len(rep.SampleUnmatched) != 1 {
		t.Errorf("壊れたペアの実例が出ていない(数だけでは銘柄の偏りが見えない)")
	}
	if rep.Verdict != "no_pairs" {
		t.Errorf("判定 = %q, want no_pairs", rep.Verdict)
	}
}

// 🛑 **事前登録した閾値の片方しか実装していなかった**。
//
// 事前登録は締めの参考閾値を
// 「`cmd/edge-judge` が実際に持っているもの(Track B で **N≥100** かつ **day_blocks≥20**)」
// と登録している。ところが pair-diff は `day_blocks >= 20` しか見ておらず、
// **20 営業日ぶんの日付がありさえすれば 25 組でも `trail_better` を印字した**。
// 本命の統計量を出す道具のほうが緩いと、採点者は緩い方を先に読む。
//
// ここで新しい数字を発明していない — judge の minN(100)をそのまま使う。
func TestVerdictRefusesToDecideBelowMinPairsEvenWithEnoughDayBlocks(t *testing.T) {
	var rows []pg.PairTradeRow
	// 25 営業日 × 1 ペア = day_blocks は足りるが N は 25 で 100 に届かない。
	for i := 0; i < 25; i++ {
		day := time.Date(2026, 8, 24, 0, 0, 0, 0, clock.JST).AddDate(0, 0, i).Format("2006-01-02")
		rows = append(rows,
			row("7203", "abs_momentum_v2", 2500, 100, -100, day),
			row("7203", "abs_momentum_v2_trail", 2500, 100, 900, day),
		)
	}
	rep := buildReport("abs_momentum_v2", rows, toTrades(rows), nil, 500, 42)

	if rep.PairsN != 25 {
		t.Fatalf("ペア = %d, want 25", rep.PairsN)
	}
	if rep.DayBlocks < minDayBlocks {
		t.Fatalf("day_blocks = %d — この検定は day_blocks が足りている前提", rep.DayBlocks)
	}
	if rep.Verdict != "insufficient_n" {
		t.Errorf("verdict = %q, want insufficient_n — trail が全勝でも N が足りなければ判定を出さない", rep.Verdict)
	}
	var found bool
	for _, why := range rep.Reasons {
		if strings.Contains(why, "100") {
			found = true
		}
	}
	if !found {
		t.Errorf("理由に閾値(100)が出ていない: %v", rep.Reasons)
	}
}
