package config

import "testing"

// config 凍結は「同じ config_id は永久に同じ内容」で成り立っている(pg の upsert は
// 既存 raw_yaml を**上書きしない**)。だから内容を変えたのに id を使い回すと、台帳に
// 残るのは**最初の版**で、実際に発注した設定と食い違う。
//
// 実際に起きた: live の armed config `live_bnf_probe_v1_4751` は
// exec_kind を margin_general → margin_system → cash → margin_system と変えても
// id が同じままで、DB には初版の margin_general が凍ったまま残っていた。
// 発注自体は in-memory の config で出るので誤発注ではないが、**トレードを説明する
// 記録が嘘になる**(エッジ台帳は config_id で結合する)。
//
// Fingerprint は「内容が変われば別 id になる」ための材料。銘柄と id 自体は
// 除外する — 同じ設定を 200 銘柄に当てたときは同じ指紋であってほしい。
func TestStrategyConfigFingerprintTracksContent(t *testing.T) {
	base := &StrategyConfig{
		ConfigID: "live_v1", Symbol: "*", StrategyName: StrategyBNFReversion,
		Mode: ModeLive, HoldingMode: HoldingMultiday, ExecKind: ExecMarginSystem,
	}
	fp := base.Fingerprint()
	if fp == "" {
		t.Fatal("Fingerprint が空")
	}

	same := *base
	if got := same.Fingerprint(); got != fp {
		t.Errorf("同じ内容で指紋が変わった: %q != %q", got, fp)
	}

	// 銘柄と id は指紋に含めない(1 テンプレを 200 銘柄に複製しても同じ設定)。
	perSym := *base
	perSym.Symbol, perSym.ConfigID = "4751", "live_v1_4751"
	if got := perSym.Fingerprint(); got != fp {
		t.Errorf("銘柄違いで指紋が変わった: %q != %q", got, fp)
	}

	// 🛑 取引の中身が変わったら必ず変わる。
	for name, mutate := range map[string]func(*StrategyConfig){
		"exec_kind":    func(c *StrategyConfig) { c.ExecKind = ExecCash },
		"strategy":     func(c *StrategyConfig) { c.StrategyName = StrategyNoTrade },
		"holding_mode": func(c *StrategyConfig) { c.HoldingMode = HoldingIntraday },
		"stop_loss":    func(c *StrategyConfig) { c.Exit.StopLossJPY = 500 },
		"quantity":     func(c *StrategyConfig) { c.Risk.Quantity = 200 },
	} {
		c := *base
		mutate(&c)
		if got := c.Fingerprint(); got == fp {
			t.Errorf("%s を変えたのに指紋が同じ (%q) — 別の設定が同じ config_id に化ける", name, got)
		}
	}
}
