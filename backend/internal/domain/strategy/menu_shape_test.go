package strategy

import (
	"sort"
	"strings"
	"testing"
)

// 🛑 **メニューの形を doc に手で書かない**。
//
// doc に「戦略が13個」や「7個 × 出口2通り」と手で書くと
// **7×2=14 で実数と合わない**。実際は:
//
//	入口 7 / うち 5 に兄弟アーム(_trail)/ 合計 12 / ペア差が取れるのは 5 組
//	ペアを持たない入口 = high_volume_premium・post_jump_drift
//
// 採点でこれがずれると、①多重検定の数を間違える ②「ペアが 7 組あるはず」と読んで
// 欠測を故障と誤診する のどちらかが起きる。**doc ではなく実装を正とする**ため、
// 数を変えたらこのテストが落ちる。落ちたら doc も同時に直すこと。
func TestMenuShapeIsEighteenArmsWithEightPairs(t *testing.T) {
	var all []string
	for _, s := range DefaultScreeners() {
		all = append(all, string(s.Screen("7203", nil).Strategy))
	}
	sort.Strings(all)

	set := map[string]bool{}
	for _, a := range all {
		if set[a] {
			t.Fatalf("メニューに重複がある: %s", a)
		}
		set[a] = true
	}

	var bases, trails, paired, lonely []string
	for _, a := range all {
		if strings.HasSuffix(a, TrailArmSuffix) {
			trails = append(trails, a)
			continue
		}
		bases = append(bases, a)
		if set[a+TrailArmSuffix] {
			paired = append(paired, a)
		} else {
			lonely = append(lonely, a)
		}
	}

	// bnf_intraday_reversion を screener に載せ、
	// その `_trail` 兄弟を足した。コードのメニューは **入口 10 / うち 8 に出口 2 通り = 18 アーム**。
	// 🛑 paper で実際に回る入口は `advisor_v2.entries`(bot_config.advisor.yaml)が絞る —
	// ここはコードのメニューの形で、稼働中の形は `research_config_guard_test` が固定する。
	if len(all) != 18 {
		t.Errorf("アーム合計=%d, want 18: %v", len(all), all)
	}
	if len(bases) != 10 {
		t.Errorf("入口=%d, want 10: %v", len(bases), bases)
	}
	if len(trails) != 8 {
		t.Errorf("兄弟アーム=%d, want 8: %v", len(trails), trails)
	}
	if len(paired) != 8 {
		t.Errorf("ペア成立=%d, want 8 — ペア差はこの数だけ取れる: %v", len(paired), paired)
	}
	// 兄弟の基がメニューに居ないと、ペア差が片脚だけで永久に成立しない。
	for _, tr := range trails {
		if !set[strings.TrimSuffix(tr, TrailArmSuffix)] {
			t.Errorf("%s の基アームがメニューに無い — ペア差が永久に取れない", tr)
		}
	}
	want := []string{"high_volume_premium", "post_jump_drift"}
	if strings.Join(lonely, ",") != strings.Join(want, ",") {
		t.Errorf("ペアを持たない入口=%v, want %v(変えたなら doc の内訳も同時に直す)", lonely, want)
	}
}
