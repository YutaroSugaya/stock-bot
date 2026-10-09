package freshness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDump(t *testing.T, dir, stamp string, size int) {
	t.Helper()
	body := make([]byte, size)
	if err := os.WriteFile(filepath.Join(dir, "stockbot-"+stamp+".sql.gz"), body, 0o644); err != nil {
		t.Fatalf("write dump %s: %v", stamp, err)
	}
}

func atUTC(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad time %q: %v", s, err)
	}
	return ts
}

func TestCheckBackupFreshness(t *testing.T) {
	const maxAge = 26 * time.Hour
	now := atUTC(t, "2026-08-10T18:10:00Z")

	t.Run("直近の dump があれば健全", func(t *testing.T) {
		dir := t.TempDir()
		writeDump(t, dir, "20260808T181003Z", 1000)
		writeDump(t, dir, "20260809T181003Z", 1000) // 24.0h 前
		got := CheckBackup(dir, nil, now, maxAge)
		if got.Stale {
			t.Errorf("stale=true, want false (age=%.1fh)", got.AgeHours)
		}
		if got.Newest != "20260809T181003Z" {
			t.Errorf("Newest=%q, want 20260809T181003Z", got.Newest)
		}
		if got.N != 2 {
			t.Errorf("N=%d, want 2", got.N)
		}
	})

	t.Run("maxAge を超えたら stale", func(t *testing.T) {
		dir := t.TempDir()
		writeDump(t, dir, "20260808T181003Z", 1000) // 48h 前 = 1回飛んだ状態
		got := CheckBackup(dir, nil, now, maxAge)
		if !got.Stale {
			t.Errorf("stale=false, want true (age=%.1fh)", got.AgeHours)
		}
	})

	// ここを false に倒すと一番まずい状態が一番静かになる(fail-close)。
	t.Run("dump が1つも無ければ stale", func(t *testing.T) {
		got := CheckBackup(t.TempDir(), nil, now, maxAge)
		if !got.Stale {
			t.Error("dump ゼロで stale=false — バックアップ皆無が健全に見える")
		}
		if got.N != 0 {
			t.Errorf("N=%d, want 0", got.N)
		}
	})

	t.Run("ディレクトリが無ければ stale", func(t *testing.T) {
		got := CheckBackup(filepath.Join(t.TempDir(), "存在しない"), nil, now, maxAge)
		if !got.Stale {
			t.Error("dir 不在で stale=false — 退避先ごと消えても気づけない")
		}
	})

	// pg_dump が途中で失敗すると 0 バイトが残りうる。壊れた dump が本物の警告を
	// 打ち消さないこと。
	t.Run("0バイトは数えない", func(t *testing.T) {
		dir := t.TempDir()
		writeDump(t, dir, "20260808T181003Z", 1000) // 48h 前(古い)
		writeDump(t, dir, "20260810T181003Z", 0)    // 直近だが空
		got := CheckBackup(dir, nil, now, maxAge)
		if !got.Stale {
			t.Error("0バイトを新しい dump として数えた — 壊れた退避が警告を消す")
		}
		if got.Newest != "20260808T181003Z" {
			t.Errorf("Newest=%q — 0バイトを最新に採用している", got.Newest)
		}
		if got.N != 1 {
			t.Errorf("N=%d, want 1(0バイトは除く)", got.N)
		}
	})

	// db-backup.sh は `$out.tmp` へ書いてから rename する。
	t.Run("書き込み途中(.tmp)は数えない", func(t *testing.T) {
		dir := t.TempDir()
		writeDump(t, dir, "20260808T181003Z", 1000)
		if err := os.WriteFile(filepath.Join(dir, "stockbot-20260810T181003Z.sql.gz.tmp"), make([]byte, 500), 0o644); err != nil {
			t.Fatalf("write tmp: %v", err)
		}
		got := CheckBackup(dir, nil, now, maxAge)
		if !got.Stale {
			t.Error(".tmp を完成した dump と数えた")
		}
	})

	// 退避先には data 側の tar.gz や手で置いたファイルも混ざる。1つの異物で監視が
	// 死なないこと。
	t.Run("無関係なファイルは無視して正常な dump を拾う", func(t *testing.T) {
		dir := t.TempDir()
		writeDump(t, dir, "20260809T181003Z", 1000)
		writeDump(t, dir, "こわれた名前", 1000)
		if err := os.WriteFile(filepath.Join(dir, "daily-20260809T220008Z.tar.gz"), make([]byte, 100), 0o644); err != nil {
			t.Fatalf("write tar: %v", err)
		}
		got := CheckBackup(dir, nil, now, maxAge)
		if got.Stale {
			t.Errorf("stale=true — 異物のせいで正常な dump を見失った (N=%d)", got.N)
		}
		if got.N != 1 {
			t.Errorf("N=%d, want 1", got.N)
		}
	})

	// ジッタで鳴かせない: ちょうど maxAge は「まだ健全」。
	t.Run("ちょうど maxAge は健全", func(t *testing.T) {
		dir := t.TempDir()
		writeDump(t, dir, "20260809T161000Z", 1000) // ちょうど 26h 前
		got := CheckBackup(dir, nil, now, maxAge)
		if got.Stale {
			t.Errorf("境界で stale=true (age=%.4fh)", got.AgeHours)
		}
	})
}

// 定期は毎日 03:10 JST。24h 以下だと正常運用でも毎日鳴き、48h 以上だと1回飛ばしても
// 翌朝に鳴かない。
func TestBackupMaxAgeCatchesOneMissedRun(t *testing.T) {
	if BackupMaxAge <= 24*time.Hour {
		t.Errorf("BackupMaxAge=%v — 正常な日次運用でも鳴く(狼少年になる)", BackupMaxAge)
	}
	if BackupMaxAge >= 48*time.Hour {
		t.Errorf("BackupMaxAge=%v — 1回飛んでも翌朝に鳴かない", BackupMaxAge)
	}
}

// 日次 dump が 1 回飛んだ朝を、実際の時刻とファイル名の形で再現する。人間が画面を
// 見る朝(07:00 JST)に鳴っていなければ意味がない。
func TestCatchesThe20260810Miss(t *testing.T) {
	dir := t.TempDir()
	writeDump(t, dir, "20260807T181004Z", 18466088)
	writeDump(t, dir, "20260808T181003Z", 18469983) // 最後に成功した dump
	// 08-10 07:00 JST = 08-09 22:00 UTC
	got := CheckBackup(dir, nil, atUTC(t, "2026-08-09T22:00:00Z"), BackupMaxAge)
	if !got.Stale {
		t.Errorf("08-10 朝に stale=false (age=%.1fh) — 実際の欠落を捕まえられていない", got.AgeHours)
	}
}

// ダッシュボードは JSON キーを素の JS で読むので、Go 側でキー名を変えてもコンパイラは
// 何も言わず、画面の警告だけが黙って消える。
func TestDashboardConsumesBackupKeys(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "handler", "web", "index.html"))
	if err != nil {
		t.Fatalf("dashboard を読めない: %v", err)
	}
	html := string(b)

	raw, err := json.Marshal(BackupStatus{Newest: "n", AgeHours: 1, N: 1, Stale: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k := range keys {
		if !strings.Contains(html, k) {
			t.Errorf("dashboard が backup の %q を読んでいない — 警告が画面から消える", k)
		}
	}
	if !strings.Contains(html, "db_backup") {
		t.Error("dashboard が db_backup 自体を読んでいない")
	}
}
