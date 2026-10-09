package strategy

import (
	"strings"
	"testing"

	"stockbot/backend/internal/config"
)

// paper で**回す入口**は config(`advisor_v2.entries`)が持ち、
// コードのメニュー(DefaultScreeners = 18 アーム)から**入口単位**で絞る。
// トレンド系はメニューから消さない(停止であって棄却ではない)ので、絞る仕組みが要る。

func names(ss []Screener) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, string(s.Name()))
	}
	return out
}

// 一覧に無い入口の screener は 1 本も通らない。兄弟(`_trail`)は入口へ畳んで一緒に通る。
func TestScreenersForEntries_KeepsOnlyListedEntriesWithTheirSiblings(t *testing.T) {
	got, err := ScreenersForEntries([]config.StrategyName{config.StrategyBNFReversion, config.StrategyPostJumpDrift})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bnf_reversion", "bnf_reversion_trail", "post_jump_drift"}
	if strings.Join(names(got), ",") != strings.Join(want, ",") {
		t.Fatalf("screeners = %v, want %v(DefaultScreeners の順序を保つ)", names(got), want)
	}
	for _, s := range got {
		if EntryArmOf(s.Name()) != config.StrategyBNFReversion && EntryArmOf(s.Name()) != config.StrategyPostJumpDrift {
			t.Fatalf("一覧に無い入口 %q が通っている", s.Name())
		}
	}
}

// 空 = 絞らない(このキーを持たない config はコードのメニュー全部で回る)。
func TestScreenersForEntries_EmptyMeansTheWholeMenu(t *testing.T) {
	got, err := ScreenersForEntries(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(DefaultScreeners()) {
		t.Fatalf("空の一覧で %d 本 — メニュー全部(%d)であること", len(got), len(DefaultScreeners()))
	}
}

// 🛑 綴りの誤り・兄弟名・重複は fail-close(黙って落とすと、そのアームが永久に標本ゼロになる)。
func TestScreenersForEntries_RejectsUnknownSiblingAndDuplicateNames(t *testing.T) {
	for _, bad := range [][]config.StrategyName{
		{"bnf_reversoin"},                                          // typo
		{config.StrategyBNFReversionTrail},                         // 兄弟名は入口ではない
		{config.StrategyMACross},                                   // メニュー外(棄却済み)
		{config.StrategyBNFReversion, config.StrategyBNFReversion}, // 重複
	} {
		if _, err := ScreenersForEntries(bad); err == nil {
			t.Errorf("%v: error を返すこと(fail-close)", bad)
		}
	}
}
