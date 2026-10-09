package pg

import (
	"testing"

	"stockbot/backend/internal/config"
)

// 🛑 `ActivateExclusive` はこの文字列だけで「一意キーを緩めるか」を決める。
// config.ModeLive とずれると、**live で 1 銘柄に複数戦略の active が並ぶ**ようになり、
// 研究モードの緩和が実弾へ静かに漏れる。値を 2 箇所に持っている以上、片方を変えても
// 両方緑で通ってしまうので、ここで縛る。
func TestLiveConfigModeMatchesTheConfigPackage(t *testing.T) {
	if liveConfigMode != string(config.ModeLive) {
		t.Fatalf("liveConfigMode = %q, want %q (config.ModeLive)", liveConfigMode, config.ModeLive)
	}
}
