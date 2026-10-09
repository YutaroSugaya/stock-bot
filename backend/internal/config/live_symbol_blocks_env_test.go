package config

import "testing"

// 既定は live の emergency フラグと同じディレクトリの live_symbol_blocks.json。
func TestLiveTrackEnv_SymbolBlocksFileDefaultsNextToEmergencyFlag(t *testing.T) {
	t.Setenv("STOCKBOT_LIVE_SYMBOL_BLOCKS", "")
	t.Setenv("STOCKBOT_LIVE_EMERGENCY_FLAG", "/home/x/.stockbot/state/emergency_stop.live.flag")
	if got := LoadLiveTrackEnv().SymbolBlocksFile(); got != "/home/x/.stockbot/state/live_symbol_blocks.json" {
		t.Fatalf("既定 = %q", got)
	}
}

func TestLiveTrackEnv_SymbolBlocksFileFromEnv(t *testing.T) {
	t.Setenv("STOCKBOT_LIVE_SYMBOL_BLOCKS", "/tmp/b.json")
	t.Setenv("STOCKBOT_LIVE_EMERGENCY_FLAG", "runtime/emergency_stop.live.flag")
	if got := LoadLiveTrackEnv().SymbolBlocksFile(); got != "/tmp/b.json" {
		t.Fatalf("env = %q", got)
	}
}
