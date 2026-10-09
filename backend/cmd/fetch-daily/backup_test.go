package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 日足 CSV は git 管理外 + pg_dump の対象外で、立花の遡及(約250本)を超えた日は
// 再取得できない = 壊したら復旧できない唯一のコピー。

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readArchive returns the archive's entries as name → content.
func readArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gz.Close()
	out := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read %s: %v", h.Name, err)
		}
		out[h.Name] = string(b)
	}
	return out
}

func TestBackupDailyCSVsArchivesDailyFilesByteForByte(t *testing.T) {
	out, backup := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(out, "7203_daily.csv"), "2026-08-04;1;2;3;4;5\n")
	writeFile(t, filepath.Join(out, "bench_topix.csv"), "2026-08-04;9;9;9;9;0\n")

	at := time.Date(2026, 8, 5, 7, 45, 0, 0, time.UTC)
	path, err := backupDailyCSVs(out, backup, at, 30)
	if err != nil {
		t.Fatalf("backupDailyCSVs: %v", err)
	}
	if want := filepath.Join(backup, "daily-20260805T074500Z.tar.gz"); path != want {
		t.Fatalf("archive path = %q, want %q", path, want)
	}
	got := readArchive(t, path)
	if got["7203_daily.csv"] != "2026-08-04;1;2;3;4;5\n" {
		t.Errorf("7203_daily.csv の中身が違う: %q", got["7203_daily.csv"])
	}
	// ベンチも fetch-daily が書き換える対象なので一緒に退避する。
	if got["bench_topix.csv"] != "2026-08-04;9;9;9;9;0\n" {
		t.Errorf("bench_topix.csv が退避されていない: %q", got["bench_topix.csv"])
	}
	if ents, _ := filepath.Glob(filepath.Join(backup, "*.tmp")); len(ents) != 0 {
		t.Errorf(".tmp が残っている: %v", ents)
	}
}

// 分足は 96MB あり、日足(2.6MB)と一緒に毎日30世代取ると数GBになる。
func TestBackupDailyCSVsSkipsMinuteBars(t *testing.T) {
	out, backup := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(out, "7203_daily.csv"), "d\n")
	writeFile(t, filepath.Join(out, "7203_5m.csv"), "m\n")
	writeFile(t, filepath.Join(out, "7203_1m.csv"), "m\n")

	path, err := backupDailyCSVs(out, backup, time.Unix(0, 0).UTC(), 30)
	if err != nil {
		t.Fatalf("backupDailyCSVs: %v", err)
	}
	got := readArchive(t, path)
	if len(got) != 1 {
		t.Fatalf("退避対象は日足のみのはず: %v", got)
	}
}

// 退避すべきものが無いのに空アーカイブを積むと、世代が空で埋まって本物の退避が
// 押し出される(退避があるのに中身が無い、が最悪の失敗)。
func TestBackupDailyCSVsWritesNothingWhenNoDailyCSVs(t *testing.T) {
	out, backup := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(out, "7203_5m.csv"), "m\n")

	path, err := backupDailyCSVs(out, backup, time.Unix(0, 0).UTC(), 30)
	if err != nil {
		t.Fatalf("backupDailyCSVs: %v", err)
	}
	if path != "" {
		t.Errorf("退避対象ゼロで archive を作った: %q", path)
	}
	ents, _ := os.ReadDir(backup)
	if len(ents) != 0 {
		t.Errorf("退避先にファイルができている: %v", ents)
	}
}

func TestBackupDailyCSVsRotatesOldestFirst(t *testing.T) {
	out, backup := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(out, "7203_daily.csv"), "d\n")
	for _, old := range []string{"daily-20260801T000000Z.tar.gz", "daily-20260802T000000Z.tar.gz"} {
		writeFile(t, filepath.Join(backup, old), "old")
	}

	at := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	if _, err := backupDailyCSVs(out, backup, at, 2); err != nil {
		t.Fatalf("backupDailyCSVs: %v", err)
	}
	ents, _ := filepath.Glob(filepath.Join(backup, "daily-*.tar.gz"))
	if len(ents) != 2 {
		t.Fatalf("世代数 = %d, want 2: %v", len(ents), ents)
	}
	if filepath.Base(ents[0]) != "daily-20260802T000000Z.tar.gz" {
		t.Errorf("消すのは最古から: 残ったのは %v", ents)
	}
}

// 🛑 **再取得できない記録**(jpx_stops*.csv)も退避する。JPX は当日分しか置かないので、
// 失ったら永久に戻らない — 日足(再取得できる)より優先度が高い。
func TestBackupTargets_IncludesUnrecoverableRecords(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"7203_daily.csv", "bench_topix.csv", "jpx_stops.csv", "jpx_stops_days.csv", "other.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := backupTargets(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"7203_daily.csv": true, "bench_topix.csv": true, "jpx_stops.csv": true, "jpx_stops_days.csv": true}
	for _, n := range got {
		if !want[n] {
			t.Errorf("退避対象に余計なものが入っている: %q", n)
		}
		delete(want, n)
	}
	if len(want) != 0 {
		t.Fatalf("退避されないファイルがある: %+v", want)
	}
}
