package pairdiff

import (
	"testing"
)

func tr(sym, strat string, entry, net float64, day string) Trade {
	return Trade{Symbol: sym, Strategy: strat, EntryPrice: entry, NetJPY: net, Day: day}
}

// 同一銘柄・同一建値・同一日 → 1 ペア。統計量は trail − capped。
func TestMatchPairsSameTriggerAndTakesTrailMinusCapped(t *testing.T) {
	res := MatchWithOpen("abs_momentum_v2", []Trade{
		tr("7203", "abs_momentum_v2", 2500, 1000, "2026-08-24"),
		tr("7203", "abs_momentum_v2_trail", 2500, 2500, "2026-08-24"),
	}, nil)
	if len(res.Pairs) != 1 {
		t.Fatalf("ペア = %d, want 1: %+v", len(res.Pairs), res.Pairs)
	}
	if got := res.Pairs[0].Diff; got != 1500 {
		t.Fatalf("Diff = %v, want 1500(trail 2500 − capped 1000)", got)
	}
	if len(res.Unmatched) != 0 {
		t.Fatalf("片側のみが出た: %+v", res.Unmatched)
	}
}

// 🚨 **片側しか建たなかったトリガーを必ず数える**。
// コスト床は trail 側だけ 3 倍厳しいので、壊れたペアは高コスト銘柄で
// 選択的に trail 側から消える。ペアが揃ったものだけ見ると、その偏りごと結論になる。
func TestMatchCountsBrokenPairsByTheMissingArm(t *testing.T) {
	res := MatchWithOpen("atr_breakout_v2", []Trade{
		// trail が床に落ちて建たなかった(= 欠けたのは trail 側)
		tr("6758", "atr_breakout_v2", 1200, 300, "2026-08-24"),
		// capped 側が別の理由で建たなかった
		tr("9984", "atr_breakout_v2_trail", 8000, -400, "2026-08-25"),
		// 揃ったペア
		tr("8306", "atr_breakout_v2", 900, 100, "2026-08-26"),
		tr("8306", "atr_breakout_v2_trail", 900, 250, "2026-08-26"),
	}, nil)
	if len(res.Pairs) != 1 {
		t.Fatalf("ペア = %d, want 1", len(res.Pairs))
	}
	broken := res.BrokenByArm()
	if broken["atr_breakout_v2_trail"] != 1 {
		t.Errorf("trail 欠けの件数 = %d, want 1 — 検出器が数えていない", broken["atr_breakout_v2_trail"])
	}
	if broken["atr_breakout_v2"] != 1 {
		t.Errorf("capped 欠けの件数 = %d, want 1", broken["atr_breakout_v2"])
	}
}

// 🛑 **建値が違えば別トリガー**。同じ銘柄・同じ日でも組にしない
// (同日に 2 回建った場合、別々の事象を 1 組にすると差が無意味になる)。
func TestMatchDoesNotPairDifferentEntryPrices(t *testing.T) {
	res := MatchWithOpen("donchian_breakout_v2", []Trade{
		tr("7203", "donchian_breakout_v2", 2500, 100, "2026-08-24"),
		tr("7203", "donchian_breakout_v2_trail", 2600, 200, "2026-08-24"),
	}, nil)
	if len(res.Pairs) != 0 {
		t.Fatalf("建値が違うのに組にした: %+v", res.Pairs)
	}
	if len(res.Unmatched) != 2 {
		t.Fatalf("片側のみ = %d, want 2", len(res.Unmatched))
	}
}

// 建値の下位桁のゆらぎ(経路差)でペアを壊さない。呼値は最小 0.1 円なので
// 0.01 円の分解能で束ねる。
func TestMatchToleratesSubTickFloatNoise(t *testing.T) {
	res := MatchWithOpen("high_52w_momentum", []Trade{
		tr("7203", "high_52w_momentum", 2500.000000001, 100, "2026-08-24"),
		tr("7203", "high_52w_momentum_trail", 2500.0, 200, "2026-08-24"),
	}, nil)
	if len(res.Pairs) != 1 {
		t.Fatalf("浮動小数の下位桁でペアが壊れた: %+v / %+v", res.Pairs, res.Unmatched)
	}
}

// 他アームのトレードが混ざっていても無視する(台帳を丸ごと渡せる)。
func TestMatchIgnoresOtherArms(t *testing.T) {
	res := MatchWithOpen("abs_momentum_v2", []Trade{
		tr("7203", "abs_momentum_v2", 2500, 100, "2026-08-24"),
		tr("7203", "abs_momentum_v2_trail", 2500, 200, "2026-08-24"),
		tr("7203", "bnf_reversion", 2500, 999, "2026-08-24"),
		tr("7203", "atr_breakout_v2", 2500, 888, "2026-08-24"),
	}, nil)
	if len(res.Pairs) != 1 || len(res.Unmatched) != 0 {
		t.Fatalf("他アームを巻き込んだ: pairs=%+v unmatched=%+v", res.Pairs, res.Unmatched)
	}
}

// 🛑 **出力は決定論**。map 走査順が漏れると、同じ台帳から実行のたびに違う CI が出る。
func TestMatchOutputIsDeterministic(t *testing.T) {
	in := []Trade{
		tr("9984", "abs_momentum_v2", 8000, 10, "2026-08-26"),
		tr("9984", "abs_momentum_v2_trail", 8000, 20, "2026-08-26"),
		tr("6758", "abs_momentum_v2", 1200, 30, "2026-08-24"),
		tr("6758", "abs_momentum_v2_trail", 1200, 40, "2026-08-24"),
		tr("8306", "abs_momentum_v2", 900, 50, "2026-08-25"),
	}
	first := MatchWithOpen("abs_momentum_v2", in, nil)
	for i := 0; i < 20; i++ {
		got := MatchWithOpen("abs_momentum_v2", in, nil)
		for j := range got.Pairs {
			if got.Pairs[j] != first.Pairs[j] {
				t.Fatalf("実行ごとに並びが変わる(%d 回目)", i)
			}
		}
		if len(got.Unmatched) != len(first.Unmatched) || got.Unmatched[0] != first.Unmatched[0] {
			t.Fatalf("Unmatched の並びが変わる(%d 回目)", i)
		}
	}
	// 日付昇順で並ぶこと。
	if first.Pairs[0].Day != "2026-08-24" || first.Pairs[1].Day != "2026-08-26" {
		t.Fatalf("日付順でない: %+v", first.Pairs)
	}
}

// day-block bootstrap へ渡す 2 本のスライスは**同順・同長**でなければならない
// (ずれるとブロックの割り当てが崩れ、CI が別物になる)。
func TestDiffsAndDaysAreParallel(t *testing.T) {
	res := MatchWithOpen("abs_momentum_v2", []Trade{
		tr("6758", "abs_momentum_v2", 1200, 30, "2026-08-24"),
		tr("6758", "abs_momentum_v2_trail", 1200, 40, "2026-08-24"),
		tr("9984", "abs_momentum_v2", 8000, 10, "2026-08-26"),
		tr("9984", "abs_momentum_v2_trail", 8000, 5, "2026-08-26"),
	}, nil)
	d, days := res.Diffs(), res.Days()
	if len(d) != len(days) || len(d) != len(res.Pairs) {
		t.Fatalf("長さが揃っていない: diffs=%d days=%d pairs=%d", len(d), len(days), len(res.Pairs))
	}
	if d[0] != 10 || days[0] != "2026-08-24" || d[1] != -5 || days[1] != "2026-08-26" {
		t.Fatalf("並行していない: %v / %v", d, days)
	}
	if res.DayBlocks != 2 {
		t.Fatalf("DayBlocks = %d, want 2", res.DayBlocks)
	}
	if res.MeanDiffJPY != 2.5 {
		t.Fatalf("MeanDiff = %v, want 2.5", res.MeanDiffJPY)
	}
}
