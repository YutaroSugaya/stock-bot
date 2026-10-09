package freshness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ダッシュボードは JSON キーを素の JS で読むので、Go 側でキー名を変えてもコンパイラは
// 何も言わず、画面の警告だけが黙って消える(= また4日間気づけない)。
func TestDashboardConsumesDailyCSVKeys(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "handler", "web", "index.html"))
	if err != nil {
		t.Fatalf("dashboard を読めない: %v", err)
	}
	html := string(b)

	raw, err := json.Marshal(DailyCSVStatus{Through: "t", Newest: "n", Oldest: "o", BehindN: 1, Behind: []string{"x"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k := range keys {
		if !strings.Contains(html, k) {
			t.Errorf("dashboard が daily_csv の %q を読んでいない — 警告が画面から消える", k)
		}
	}
	if !strings.Contains(html, "daily_csv") {
		t.Error("dashboard が daily_csv 自体を読んでいない")
	}
}

func writeDailyCSV(t *testing.T, dir, sym, lastDay string) {
	t.Helper()
	body := "2026-07-31;100;101;99;100;1000\n" + lastDay + ";100;101;99;100;1000\n"
	if err := os.WriteFile(filepath.Join(dir, sym+"_daily.csv"), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", sym, err)
	}
}

func TestCheckDailyCSVFreshnessCountsSymbolsBehind(t *testing.T) {
	dir := t.TempDir()
	writeDailyCSV(t, dir, "7203", "2026-08-04")
	writeDailyCSV(t, dir, "6758", "2026-08-01") // 3営業日ぶん遅れている
	writeDailyCSV(t, dir, "8802", "2026-08-04")

	got := CheckDailyCSV(dir, []string{"7203", "6758", "8802"}, "2026-08-04", nil)
	if got.BehindN != 1 {
		t.Fatalf("BehindN = %d, want 1 (%+v)", got.BehindN, got)
	}
	if len(got.Behind) != 1 || got.Behind[0] != "6758" {
		t.Errorf("Behind = %v, want [6758]", got.Behind)
	}
	if got.Newest != "2026-08-04" {
		t.Errorf("Newest = %q, want 2026-08-04", got.Newest)
	}
	if got.Oldest != "2026-08-01" {
		t.Errorf("Oldest = %q, want 2026-08-01", got.Oldest)
	}
}

// universe だけを見ると監視の穴になる。2件すり抜けた: 7週間古い
// bench_topix.csv(edge-eval の**ベンチ超過ゲート**の入力なので、古いと「TOPIX に
// 勝てていない戦略」を勝ちと判定しうる)と、売買停止で universe から外したあと
// 2026-05-29 で止まっていた 6201。
func TestCheckDailyCSVFreshnessWatchesFilesOutsideTheUniverse(t *testing.T) {
	dir := t.TempDir()
	writeDailyCSV(t, dir, "7203", "2026-08-04") // universe 内・最新
	writeDailyCSV(t, dir, "6201", "2026-05-29") // universe 外・古い
	if err := os.WriteFile(filepath.Join(dir, "bench_topix.csv"),
		[]byte("2026-06-18;1;1;1;1;1\n"), 0o644); err != nil {
		t.Fatalf("bench: %v", err)
	}

	got := CheckDailyCSV(dir, []string{"7203"}, "2026-08-04", nil)
	if got.BehindN != 2 {
		t.Fatalf("BehindN = %d, want 2 (6201 と bench): %+v", got.BehindN, got)
	}
	names := map[string]bool{}
	for _, n := range got.Behind {
		names[n] = true
	}
	if !names["6201"] || !names["bench_topix"] {
		t.Errorf("universe 外 / bench を見ていない: %v", got.Behind)
	}
	if got.Oldest != "2026-05-29" {
		t.Errorf("Oldest = %q, want 2026-05-29(6201 が最古)", got.Oldest)
	}
	// universe(1)+ ディレクトリの 6201 + bench = 3。
	if got.WatchedN != 3 {
		t.Errorf("WatchedN = %d, want 3", got.WatchedN)
	}
}

// ファイルが無い銘柄を「遅れていない」に数えると、CSV がごっそり欠けているのに
// 「全部最新」と表示される。無い = 最も遅れている、として数える。
func TestCheckDailyCSVFreshnessTreatsMissingFileAsBehind(t *testing.T) {
	dir := t.TempDir()
	writeDailyCSV(t, dir, "7203", "2026-08-04")

	got := CheckDailyCSV(dir, []string{"7203", "9999"}, "2026-08-04", nil)
	if got.BehindN != 1 || len(got.Behind) != 1 || got.Behind[0] != "9999" {
		t.Fatalf("欠けている銘柄を数えていない: %+v", got)
	}
}

// 休場カレンダー失効で判定できないのに「遅れている」と鳴らすと、本物の遅れと
// 区別がつかなくなる。
func TestCheckDailyCSVFreshnessStaysQuietWhenTradingDayUnknown(t *testing.T) {
	dir := t.TempDir()
	writeDailyCSV(t, dir, "7203", "2026-07-01")

	got := CheckDailyCSV(dir, []string{"7203"}, "", nil)
	if got.BehindN != 0 {
		t.Errorf("判定不能なのに遅れとして数えた: %+v", got)
	}
	if got.Newest != "2026-07-01" {
		t.Errorf("Newest は判定不能でも出す: %q", got.Newest)
	}
}

// 222 銘柄が一斉に遅れても画面が読めること(件数は正確に、名前は先頭だけ)。
func TestCheckDailyCSVFreshnessCapsTheNameList(t *testing.T) {
	dir := t.TempDir()
	syms := make([]string, 0, dailyCSVBehindNameCap+5)
	for i := 0; i < dailyCSVBehindNameCap+5; i++ {
		sym := string(rune('A'+i%26)) + string(rune('a'+i/26))
		writeDailyCSV(t, dir, sym, "2026-07-01")
		syms = append(syms, sym)
	}
	got := CheckDailyCSV(dir, syms, "2026-08-04", nil)
	if got.BehindN != len(syms) {
		t.Errorf("BehindN = %d, want %d", got.BehindN, len(syms))
	}
	if len(got.Behind) != dailyCSVBehindNameCap {
		t.Errorf("名前リストが %d 件(cap %d)", len(got.Behind), dailyCSVBehindNameCap)
	}
}

// 🚨 上場廃止で allowed_symbols から外した銘柄の CSV が
// retired/ へ移すまで「最新でない」と鳴り続け、本物の遅れを隠した。**プールから外した銘柄の CSV は
// 数えない**。プールに残っている銘柄(日次ユニバースから外れただけの銘柄)は従来どおり数える。
func TestCheckDailyCSVFreshnessIgnoresSymbolsRemovedFromThePool(t *testing.T) {
	dir := t.TempDir()
	writeDailyCSV(t, dir, "7203", "2026-09-29")
	writeDailyCSV(t, dir, "9508", "2026-09-28") // 上場廃止でプールから外した
	writeDailyCSV(t, dir, "6201", "2026-05-29") // ユニバース外だがプールには残る
	inPool := func(s string) bool { return s != "9508" }

	got := CheckDailyCSV(dir, []string{"7203"}, "2026-09-29", inPool)
	if got.BehindN != 1 || got.Behind[0] != "6201" {
		t.Fatalf("behind = %+v, want [6201] だけ(9508 はプール外なので数えない)", got)
	}
	if got.WatchedN != 2 {
		t.Errorf("WatchedN = %d, want 2", got.WatchedN)
	}
}
