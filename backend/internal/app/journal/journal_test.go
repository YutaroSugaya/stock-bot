package journal_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stockbot/backend/internal/app/journal"
)

func writeCSV(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 実データと同じ形式(セミコロン区切り・ヘッダ無し・日足は日付だけ)。
const header = ""

// 市況は当日の日足から組む。**前日終値との比**なので、前日バーが無い銘柄は数えない。
func TestBuildMarket_CountsAdvancersFromDailyBars(t *testing.T) {
	dir := t.TempDir()
	writeCSV(t, dir, "7203_daily.csv", header+
		"2026-08-13;100;100;100;100;1\n"+
		"2026-08-14;100;110;100;110;1\n")
	writeCSV(t, dir, "6501_daily.csv", header+
		"2026-08-13;200;200;200;200;1\n"+
		"2026-08-14;200;200;180;180;1\n")
	// 当日バーが無い銘柄は**黙って 0% にしない**(分母から外す)。
	writeCSV(t, dir, "9984_daily.csv", header+"2026-08-13;300;300;300;300;1\n")
	writeCSV(t, dir, "bench_topix.csv", header+
		"2026-08-13;2000;2000;2000;2000;1\n"+
		"2026-08-14;2000;2020;2000;2020;1\n")
	uni := filepath.Join(dir, "today.txt")
	if err := os.WriteFile(uni, []byte("7203\n6501\n9984\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := journal.BuildMarket(dir, uni, "2026-08-14")
	if got.Unavailable != "" {
		t.Fatalf("unavailable = %q", got.Unavailable)
	}
	if got.UniverseN != 2 || got.Advancing != 1 || got.Declining != 1 {
		t.Fatalf("騰落 = %+v(当日バーの無い銘柄は数えない)", got)
	}
	if got.Bench == nil || got.Bench.ChangePct < 0.99 || got.Bench.ChangePct > 1.01 {
		t.Fatalf("TOPIX = %+v, want +1.00%%", got.Bench)
	}
}

// 当日の日足がまだ無い日(引け直後)は「取得できず」と書く。**前日の数字を当日として
// 出さない** — 想像で埋めないのが日次総評の規律。
func TestBuildMarket_SaysUnavailableWhenTodaysBarsMissing(t *testing.T) {
	dir := t.TempDir()
	writeCSV(t, dir, "7203_daily.csv", header+"2026-08-13;100;100;100;100;1\n")
	uni := filepath.Join(dir, "today.txt")
	if err := os.WriteFile(uni, []byte("7203\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := journal.BuildMarket(dir, uni, "2026-08-14")
	if got.Unavailable == "" {
		t.Fatalf("当日バーが無いのに数字が出ている: %+v", got)
	}
	if got.Advancing != 0 || got.Declining != 0 {
		t.Fatalf("騰落を埋めてはいけない: %+v", got)
	}
}

func TestReadUniverse_SkipsBlanksAndComments(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "u.txt")
	if err := os.WriteFile(p, []byte("# 2026-08-14\n7203\n\n 6501 \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := journal.ReadUniverse(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "7203" || got[1] != "6501" {
		t.Fatalf("universe = %+v", got)
	}
	if _, err := journal.ReadUniverse(filepath.Join(dir, "empty.txt")); err == nil {
		t.Fatal("読めないユニバースは error(静かに空で進まない)")
	}
}

// 日記であることの警告文は**機械が毎回入れる**(人間が消さない)。
func TestNote_MentionsItIsNotEvidence(t *testing.T) {
	for _, want := range []string{"エッジの証拠ではない", "edge-judge"} {
		if !strings.Contains(journal.Note, want) {
			t.Fatalf("警告文に %q が無い: %q", want, journal.Note)
		}
	}
}
