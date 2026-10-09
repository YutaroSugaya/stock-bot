package candlecsv

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 日足は日付だけの行(JST)、分足は時刻つき。ヘッダ行は読み飛ばす。時刻は UTC で持つ。
func TestLoad_DailyAndIntraday(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "7203_daily.csv", "Date;Open;High;Low;Close;Volume\n2026-06-22;2921.5;2951;2897.5;2939;24733500\n")
	cs, err := Load(p, "7203", 24*time.Hour)
	if err != nil || len(cs) != 1 {
		t.Fatalf("Load = %v, %v", cs, err)
	}
	want := time.Date(2026, 6, 22, 0, 0, 0, 0, clock.JST).UTC()
	if !cs[0].OpenTime.Equal(want) || cs[0].OpenTime.Location() != time.UTC || cs[0].Close != 2939 || cs[0].Volume != 24733500 {
		t.Fatalf("bar = %+v", cs[0])
	}
	m := write(t, dir, "7203_5m.csv", "2026-06-22 09:05:00;1;2;0.5;1.5;10\n")
	ms, err := Load(m, "7203", 5*time.Minute)
	if err != nil || len(ms) != 1 || !ms[0].OpenTime.Equal(time.Date(2026, 6, 22, 9, 5, 0, 0, clock.JST)) {
		t.Fatalf("intraday = %+v, %v", ms, err)
	}
}

func TestLoad_MissingFileIsAnError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "x.csv"), "x", 24*time.Hour); err == nil {
		t.Fatal("無いファイルを読めたことにした")
	}
}

// `<dir>/<sym>_daily.csv` の命名と列挙はここ 1 か所(bench_topix.csv 等の別名は列挙しない)。
func TestDailyFileAndSymbols(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "8306_daily.csv", "2026-06-22;1;1;1;1;1\n")
	write(t, dir, "7203_daily.csv", "2026-06-22;1;1;1;1;1\n")
	write(t, dir, "bench_topix.csv", "2026-06-22;1;1;1;1;1\n")
	if got := DailyFile(dir, "7203"); got != filepath.Join(dir, "7203_daily.csv") {
		t.Fatalf("DailyFile = %q", got)
	}
	syms, err := DailySymbols(dir)
	if err != nil || !reflect.DeepEqual(syms, []string{"7203", "8306"}) {
		t.Fatalf("DailySymbols = %v, %v(昇順・別名なし)", syms, err)
	}
	cs, err := LoadDaily(dir, "8306")
	if err != nil || len(cs) != 1 || cs[0].Symbol != "8306" {
		t.Fatalf("LoadDaily = %+v, %v", cs, err)
	}
}
