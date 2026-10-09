package freshness

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeDumpAt(t *testing.T, dir, db string, ts time.Time) {
	t.Helper()
	name := db + "-" + ts.UTC().Format(dumpStampLayout) + dumpSuffix
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 🚨 **一度も dump されていない DB は視界に入らなかった。**
//
// 「1 本でも取れたら以後は監視対象」という規則は、まだ作られていない DB で鳴かない
// ためのものだが、そのせいで **新しいサイクルの DB が「dump されない × 監視されない」
// に同時に落ちる**(実際に新しい DB が無保護のまま
// 走りかけた)。プロセスが**実際に使っている** DB 名を渡して、dump がゼロなら鳴らす。
func TestBackupFreshness_FlagsExpectedDBsWithNoDumpAtAll(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	writeDumpAt(t, dir, "stockbot", now.Add(-2*time.Hour))

	// research だけ dump があり、c3 と live は一度も取られていない。
	s := CheckBackup(dir, []string{"stockbot", "stockbot_c3", "stockbot_live"}, now, BackupMaxAge)
	if !s.Stale {
		t.Fatal("dump が一度も無い DB があるのに stale でない — 無保護のまま緑になる")
	}
	want := map[string]bool{"stockbot_c3": true, "stockbot_live": true}
	if len(s.MissingDBs) != 2 {
		t.Fatalf("MissingDBs = %v, want stockbot_c3 / stockbot_live", s.MissingDBs)
	}
	for _, db := range s.MissingDBs {
		if !want[db] {
			t.Errorf("MissingDBs に %q が混ざっている", db)
		}
	}
}

// 期待 DB を渡さない構成(既存の呼び出し)は従来どおり。
func TestBackupFreshness_NoExpectedListKeepsOldBehaviour(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	writeDumpAt(t, dir, "stockbot", now.Add(-2*time.Hour))
	if s := CheckBackup(dir, nil, now, BackupMaxAge); s.Stale {
		t.Fatalf("期待 DB を渡していないのに鳴った: %+v", s)
	}
}

// 全部揃っていれば静か。
func TestBackupFreshness_AllExpectedPresent(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	for _, db := range []string{"stockbot", "stockbot_c3", "stockbot_live"} {
		writeDumpAt(t, dir, db, now.Add(-2*time.Hour))
	}
	s := CheckBackup(dir, []string{"stockbot", "stockbot_c3", "stockbot_live"}, now, BackupMaxAge)
	if s.Stale || len(s.MissingDBs) != 0 {
		t.Fatalf("全 DB の dump があるのに鳴った: %+v", s)
	}
}
