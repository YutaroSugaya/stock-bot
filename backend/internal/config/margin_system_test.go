package config

import "testing"

// 🛑 「立花は現物+一般信用のみ」は**実口座では誤り**
// (一般信用の発注が拒否される)。立花で通るのは **現物と制度信用**。
// 一般信用は定義上存在するが口座で通らないので、起動時に落とす。
func TestSupportsExecKind_Tachibana(t *testing.T) {
	for _, k := range []BrokerKind{BrokerTachibana, BrokerPaperLiveFeed} {
		for _, c := range []struct {
			ek   ExecKind
			want bool
		}{
			{ExecCash, true},
			{ExecMarginSystem, true},
			{ExecMarginGeneral, false}, // 口座で拒否される(実測)
			{ExecMarginOneday, false},  // API に区分が無い
		} {
			if got := k.SupportsExecKind(c.ek); got != c.want {
				t.Errorf("%s.SupportsExecKind(%s)=%v, want %v", k, c.ek, got, c.want)
			}
		}
	}
}
