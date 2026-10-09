package order

import "testing"

// 🛑 制度信用。**実口座で確かめた事実**にコードを合わせる:
// 立花 e支店 v4r9 の `sGenkinShinyouKubun` は 制度信用6ヶ月(新規2/返済4)と
// 一般信用6ヶ月(新規6/返済8)の両方を定義しているが、**口座で通るのは制度信用**
// だった(一般信用で発注すると「現金信用区分に誤りがあります」で拒否される)。
// 公式サンプルの信用注文6例も全て制度信用。
func TestExecKindMarginSystem(t *testing.T) {
	if ExecMarginSystem != "margin_system" {
		t.Errorf("ExecMarginSystem=%q", ExecMarginSystem)
	}
	// 信用かどうかの判定は 1 か所に集約する。綴りが増えるたびに if を書き足すと
	// 必ずどこかが漏れる(維持率ポーリング・carry・collateral が別々に判定していた)。
	for _, c := range []struct {
		ek   ExecKind
		want bool
	}{
		{ExecCash, false},
		{ExecMarginGeneral, true},
		{ExecMarginSystem, true},
		{ExecMarginOneday, true},
	} {
		if got := c.ek.IsMargin(); got != c.want {
			t.Errorf("%s.IsMargin()=%v, want %v", c.ek, got, c.want)
		}
	}
}
