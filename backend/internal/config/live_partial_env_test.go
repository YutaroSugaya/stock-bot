package config

import (
	"strings"
	"testing"
)

// live も同じ穴を持っていた。opt-in が bot_config だけなので、
// 他の LIVE_* を書いてこれだけ忘れると **実弾建玉が誰にも監視されないまま残る**。
func TestLivePartialEnvIsRejected(t *testing.T) {
	for name, e := range map[string]LiveTrackEnv{
		"DSN だけ":           {DatabaseURL: "postgres://x/stockbot_live"},
		"emergency だけ":     {EmergencyFlagPath: "/tmp/l.flag"},
		"BOT_CONFIG だけが無い": {DatabaseURL: "postgres://x/stockbot_live", EmergencyFlagPath: "/tmp/l.flag"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := e.Validate(); err == nil {
				t.Fatal("live の部分構成が黙って無効化された — 実弾建玉が無監視になる")
			}
		})
	}
	if err := (LiveTrackEnv{}).Validate(); err != nil {
		t.Fatalf("live を使わない構成が落ちた: %v", err)
	}
	full := LiveTrackEnv{BotConfigPath: "/x/live.yaml", DatabaseURL: "postgres://x/stockbot_live", EmergencyFlagPath: "/tmp/l.flag"}
	if err := full.Validate(); err != nil {
		t.Fatalf("完全な構成が落ちた: %v", err)
	}
}

// 🛑 **明示の opt-out がある**。`.env` は「LIVE_BOT_CONFIG の 1 行をコメントアウト
// すれば live だけ無効になる」と案内しているので、そこで部分構成として落とすと
// **紙 2 トラックごと起動不能**になる。
// 「止めたい」と「書き忘れた」を区別する。
func TestLiveDisabledIsAnExplicitOptOut(t *testing.T) {
	partial := LiveTrackEnv{DatabaseURL: "postgres://x/stockbot_live", EmergencyFlagPath: "/tmp/l.flag"}
	if err := partial.Validate(); err == nil {
		t.Fatal("部分構成が素通りした")
	}
	// エラー文が**両方の直し方**を示すこと(片方しか書いていないと詰む)。
	msg := partial.Validate().Error()
	for _, want := range []string{"STOCKBOT_LIVE_DISABLED=1", "STOCKBOT_LIVE_BOT_CONFIG"} {
		if !strings.Contains(msg, want) {
			t.Errorf("エラー文に %q が無い: %s", want, msg)
		}
	}
	opted := partial
	opted.Disabled = true
	if err := opted.Validate(); err != nil {
		t.Fatalf("明示的に止めた構成が落ちた: %v", err)
	}
}
