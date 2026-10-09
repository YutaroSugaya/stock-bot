package main

import (
	"context"
	"strings"
	"testing"

	"stockbot/backend/internal/testutil"
)

// 🛑 live track に in-memory フォールバックは無い。記録が消える live は存在しては
// ならない(再起動で daily-loss 台帳・凍結 config・建玉が全部飛ぶ)。
// research 側の buildStore は DSN 未設定で in-memory に落ちるので、**同じ関数を
// 流用してはいけない**。
func TestBuildLiveStore_RefusesEmptyDSN(t *testing.T) {
	_, err := buildLiveStore(context.Background(), "", testutil.SilentLogger())
	if err == nil {
		t.Fatal("DSN 空で live store が組み立てられてしまった(in-memory へ縮退した可能性)")
	}
	if !strings.Contains(err.Error(), "STOCKBOT_LIVE_DATABASE_URL") {
		t.Errorf("err=%q が何を設定すべきか言っていない", err)
	}
}
