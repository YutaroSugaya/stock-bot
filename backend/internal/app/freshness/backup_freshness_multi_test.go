package freshness

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 🚨 `db-backup.sh` は長らく `DB_NAME=stockbot` 固定で、
// **`stockbot_live` は既にバックアップ対象外**だった。3 DB を回す形に直した以上、
// 鮮度監視も 3 本を見なければ「c3 だけ止まっている」を見逃す —— しかも c3 は
// **研究の測定そのもの**の台帳。dump は trades / positions の唯一のコピー
// (日足 CSV と違い再生成できない)。

func writeDBDump(t *testing.T, dir, db, stamp string) {
	t.Helper()
	p := filepath.Join(dir, db+"-"+stamp+".sql.gz")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func TestBackupFreshnessGoesStaleWhenAnyDatabaseFallsBehind(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 24, 6, 0, 0, 0, time.UTC)
	fresh := now.Add(-2 * time.Hour).Format(dumpStampLayout)
	old := now.Add(-100 * time.Hour).Format(dumpStampLayout)

	writeDBDump(t, dir, "stockbot", fresh)
	writeDBDump(t, dir, "stockbot_live", fresh)
	writeDBDump(t, dir, "stockbot_c3", old) // ← ここだけ止まっている

	got := CheckBackup(dir, nil, now, BackupMaxAge)
	if !got.Stale {
		t.Fatalf("1 本だけ古い状態を stale と報告していない: %+v", got)
	}
	if len(got.StaleDBs) != 1 || got.StaleDBs[0] != "stockbot_c3" {
		t.Fatalf("どの DB が古いかを報告していない: %+v", got.StaleDBs)
	}
	// 最も古い DB の経過時間を代表値にする(一番まずい状態を隠さない)。
	if got.AgeHours < 99 {
		t.Fatalf("age_hours = %v — 新しい方の DB で上書きされている", got.AgeHours)
	}
}

func TestBackupFreshnessIsHealthyWhenEveryDatabaseIsCurrent(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 24, 6, 0, 0, 0, time.UTC)
	fresh := now.Add(-2 * time.Hour).Format(dumpStampLayout)
	for _, db := range []string{"stockbot", "stockbot_c3", "stockbot_live"} {
		writeDBDump(t, dir, db, fresh)
	}
	got := CheckBackup(dir, nil, now, BackupMaxAge)
	if got.Stale || len(got.StaleDBs) != 0 {
		t.Fatalf("全部新しいのに stale: %+v", got)
	}
	if got.N != 3 {
		t.Fatalf("dumps = %d, want 3", got.N)
	}
}

// まだ作られていない DB(dump が 1 本も無い)は監視できない。**鳴らさない** —
// サイクルごとに DB を切る運用では「これから作る DB」が常に一覧に居るので、
// 不在で鳴らすと狼少年になる。1 本でも取れたら以後は監視対象。
func TestBackupFreshnessIgnoresDatabasesThatHaveNeverBeenDumped(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 24, 6, 0, 0, 0, time.UTC)
	writeDBDump(t, dir, "stockbot", now.Add(-2*time.Hour).Format(dumpStampLayout))

	got := CheckBackup(dir, nil, now, BackupMaxAge)
	if got.Stale {
		t.Fatalf("未作成の DB で鳴いている: %+v", got)
	}
}
