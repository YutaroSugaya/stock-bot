package config

import (
	"reflect"
	"testing"
)

// live のテンプレートは **複数持てる**(優先順位つき)。`STOCKBOT_LIVE_STRATEGY_CONFIG` を
// カンマ区切りにし、**並び順がそのまま優先順位**になる
// (「bnf の発火を最優先し、枠に余裕があるときに限り donchian_v2_trail」)。
//
// 🛑 並び順を優先順位にしたのは、優先度を別の設定項目に切り出すと **2 箇所が食い違う**
// 形を作るから(「ファイルは 2 本あるが優先度表には 1 本しか無い」など)。1 本しか
// 書かなければ従来と完全に同じ挙動になる(後方互換)。
func TestLiveTrackEnv_StrategyConfigPathsSplitsOnComma(t *testing.T) {
	e := LiveTrackEnv{StrategyConfigPath: "a.yaml,b.yaml"}
	got := e.StrategyConfigPaths()
	if want := []string{"a.yaml", "b.yaml"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("StrategyConfigPaths = %v, want %v", got, want)
	}
}

// 単一パスは従来どおり(後方互換)。
func TestLiveTrackEnv_StrategyConfigPathsSinglePath(t *testing.T) {
	e := LiveTrackEnv{StrategyConfigPath: "only.yaml"}
	if got := e.StrategyConfigPaths(); !reflect.DeepEqual(got, []string{"only.yaml"}) {
		t.Fatalf("StrategyConfigPaths = %v", got)
	}
}

// 空白と空要素は落とす(`a.yaml, b.yaml` や末尾カンマを人間が書く)。
func TestLiveTrackEnv_StrategyConfigPathsTrimsBlanks(t *testing.T) {
	e := LiveTrackEnv{StrategyConfigPath: " a.yaml , , b.yaml ,"}
	if got := e.StrategyConfigPaths(); !reflect.DeepEqual(got, []string{"a.yaml", "b.yaml"}) {
		t.Fatalf("StrategyConfigPaths = %v", got)
	}
}

// 未設定は空(live track 無効の判定は呼び手側の既存経路のまま)。
func TestLiveTrackEnv_StrategyConfigPathsEmpty(t *testing.T) {
	if got := (LiveTrackEnv{}).StrategyConfigPaths(); len(got) != 0 {
		t.Fatalf("StrategyConfigPaths = %v, want empty", got)
	}
}

// 戦略ごとの建玉金額上限。載っていない戦略は全体の既定に倒す。
func TestSelectorCfg_NotionalCapFor(t *testing.T) {
	c := SelectorCfg{
		MaxPositionNotionalJPY: 550000,
		PerStrategyNotionalJPY: map[string]int{string(StrategyDonchianBreakoutV2Trail): 300000},
	}
	if got := c.NotionalCapFor(StrategyBNFReversion); got != 550000 {
		t.Fatalf("bnf = %d, want 550000(既定に倒す)", got)
	}
	if got := c.NotionalCapFor(StrategyDonchianBreakoutV2Trail); got != 300000 {
		t.Fatalf("donchian = %d, want 300000(個別指定)", got)
	}
}
