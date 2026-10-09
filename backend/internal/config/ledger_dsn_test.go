package config

import (
	"strings"
	"testing"
)

// 読み出し系 cmd(forward-report / pair-diff / holding-period / daily-review / counterfactual)の
// `-db research|live|harvest` はこの 1 つで解決する。
func TestLedgerDSNResolvesEveryTrack(t *testing.T) {
	t.Setenv("STOCKBOT_DATABASE_URL", "postgres://x/research")
	t.Setenv("STOCKBOT_HARVEST_DATABASE_URL", "postgres://x/harvest")
	t.Setenv("STOCKBOT_LIVE_DATABASE_URL", "postgres://x/live")
	for db, want := range map[string]string{
		"": "research", "research": "research", "harvest": "harvest", "live": "live",
	} {
		got, err := LedgerDSN(db)
		if err != nil || !strings.HasSuffix(got, want) {
			t.Errorf("-db %q = %q (err=%v), want …/%s", db, got, err, want)
		}
	}
	if _, err := LedgerDSN("both"); err == nil {
		t.Error("未知の -db を通した")
	}
}

// 🛑 未設定の台帳は**既定へ落ちず error**(取り違えを静かに通さない)。live を既定にしない
// のも同じ理由 — 空の live DB を読んで「取引ゼロ」と報告するより、明示させる方が誤読が起きない。
func TestLedgerDSNFailsClosedWhenUnset(t *testing.T) {
	t.Setenv("STOCKBOT_DATABASE_URL", "postgres://x/research")
	t.Setenv("STOCKBOT_HARVEST_DATABASE_URL", "")
	t.Setenv("STOCKBOT_LIVE_DATABASE_URL", "")
	for _, db := range []string{"harvest", "live"} {
		if _, err := LedgerDSN(db); err == nil {
			t.Errorf("-db %s が未設定なのに通った — research の台帳を %s と誤読する", db, db)
		}
	}
	t.Setenv("STOCKBOT_DATABASE_URL", "")
	if _, err := LedgerDSN(""); err == nil {
		t.Error("STOCKBOT_DATABASE_URL 未設定なのに通った")
	}
}
