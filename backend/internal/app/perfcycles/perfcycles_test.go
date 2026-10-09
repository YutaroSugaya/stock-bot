package perfcycles

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/query"
)

type nopTrades struct{}

func (nopTrades) ListClosedSince(context.Context, time.Time) ([]port.TradeRecord, error) {
	return nil, nil
}

func noOpts(port.TradeStrategyResolver) []query.ForwardReportOption { return nil }

func jstDay(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, clock.JST) }

func mustLoad(t *testing.T) Table {
	t.Helper()
	tbl, err := Load()
	if err != nil {
		t.Fatalf("埋め込みの期間表が読めない: %v", err)
	}
	return tbl
}

// 期間表は cycles.yaml(データ)。現在の期間は最後の行で、キーは一意。
func TestLoadParsesTheEmbeddedTable(t *testing.T) {
	tbl := mustLoad(t)
	if len(tbl.Cycles) == 0 {
		t.Fatal("期間が 1 本も無い")
	}
	if last := tbl.Cycles[len(tbl.Cycles)-1]; last.Key != tbl.Current {
		t.Fatalf("current = %s だが最後の行は %s — 期間を区切ったら行を足して current を移す", tbl.Current, last.Key)
	}
}

// 壊れた表(日付・重複キー・current の不一致)は Load が error にする(画面で黙って欠けない)。
func TestParseRejectsBrokenTables(t *testing.T) {
	for _, y := range []string{
		"current: c1\ncycles:\n  - {key: c1, label: a, db: x, since: \"2026-13-01\"}\n",
		"current: c1\ncycles:\n  - {key: c1, label: a, db: x, since: \"2026-07-28\"}\n  - {key: c1, label: b, db: x, since: \"2026-08-10\"}\n",
		"current: c1\ncycles:\n  - {key: c1, label: a, db: x, since: \"2026-07-28\"}\n  - {key: c2, label: b, db: x, since: \"2026-08-10\"}\n",
		"current: c1\ncycles: []\n",
	} {
		if _, err := parse([]byte(y)); err == nil {
			t.Errorf("壊れた表を通した:\n%s", y)
		}
	}
}

// 画面の既定期間の開始日は、朝ジョブの forward-report(STOCKBOT_CYCLE_SINCE)と同じ境界。
func TestCurrentMatchesRoutineCycleSince(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "scripts", "stockbot-routine.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`STOCKBOT_CYCLE_SINCE="\$\{STOCKBOT_CYCLE_SINCE:-(\d{4}-\d{2}-\d{2})\}"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("STOCKBOT_CYCLE_SINCE の既定値が見つからない")
	}
	tbl := mustLoad(t)
	last := tbl.Cycles[len(tbl.Cycles)-1]
	if last.Since != string(m[1]) {
		t.Fatalf("現在の期間 = %s(%s)、朝ジョブの境界 = %s — 期間を区切ったら両方を更新する", last.Key, last.Since, m[1])
	}
}

// 過去の期間の DB は research と同じサーバの別 database を**読み取り専用のセッション**で開く。
func TestArchiveDSNSwitchesDatabaseAndForcesReadOnly(t *testing.T) {
	got, err := ArchiveDSN("postgres://u:p@localhost:5434/stockbot_c4?sslmode=disable", "stockbot_c3")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got)
	if u.Path != "/stockbot_c3" || u.User.String() != "u:p" || u.Host != "localhost:5434" {
		t.Fatalf("DSN = %s", got)
	}
	if u.Query().Get("sslmode") != "disable" || u.Query().Get("default_transaction_read_only") != "on" {
		t.Fatalf("既存パラメータ保持 / 読み取り専用の強制ができていない: %s", got)
	}
	if _, err := ArchiveDSN("::bad", "x"); err == nil {
		t.Fatal("解釈できない DSN を通した")
	}
}

func testDefs() []Def {
	return []Def{
		{Key: "c1", Label: "c1", DB: "stockbot", Since: "2026-07-28"},
		{Key: "c2", Label: "c2", DB: "stockbot", Since: "2026-08-10"},
		{Key: "c3", Label: "c3", DB: "stockbot_c3", Since: "2026-08-24"},
		{Key: "c4", Label: "c4", DB: "stockbot_c4", Since: "2026-09-14"},
	}
}

func TestBuildUsesEachCyclesLedgerAndRange(t *testing.T) {
	src := func() repository.TradeSource { return repository.TradeSource{Trades: nopTrades{}} }
	sources := map[string]repository.TradeSource{"stockbot": src(), "stockbot_c3": src(), "stockbot_c4": src()}
	cycles := Build(testDefs(), sources, "stockbot_c4", noOpts)
	keys := []string{}
	for _, c := range cycles {
		keys = append(keys, c.Key)
	}
	if want := []string{"c1", "c2", "c3", "c4", "all"}; len(keys) != len(want) || keys[0] != "c1" || keys[4] != "all" {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	c1, c2, c4, all := cycles[0], cycles[1], cycles[3], cycles[4]
	// c1 と c2 は同じ DB。c1 は c2 の開始日で切る。
	if !c1.Until.Equal(jstDay(2026, 8, 10)) || c1.Report == nil {
		t.Fatalf("c1 until=%v report=%v", c1.Until, c1.Report)
	}
	// c2 は DB の残り全部(上限なし)。
	if !c2.Until.IsZero() {
		t.Fatalf("c2 until = %v, want 上限なし", c2.Until)
	}
	// 現在の期間は research 自身の台帳(Report nil)。
	if c4.Report != nil || !c4.Since.Equal(jstDay(2026, 9, 14)) {
		t.Fatalf("c4 report=%v since=%v", c4.Report, c4.Since)
	}
	if all.Report == nil || !all.Since.IsZero() || !all.Until.IsZero() {
		t.Fatalf("全期間は DB を合算して期間で切らない: %+v", all)
	}

	// 過去 DB を開けなかった構成(in-memory / DB 欠落)は、その期間を出さない(0 件の画面を作らない)。
	only := Build(testDefs(), map[string]repository.TradeSource{}, "stockbot_c4", noOpts)
	if len(only) != 2 || only[0].Key != "c4" || only[1].Key != All || only[1].Report != nil {
		t.Fatalf("過去 DB なし = %+v", only)
	}
}

// live は同じ live DB を日付で切る。境界は次の期間の開始日。
func TestBuildLiveCutsByNextCycleStart(t *testing.T) {
	cycles := BuildLive(testDefs())
	byKey := map[string]int{}
	for i, c := range cycles {
		byKey[c.Key] = i
		if c.Report != nil {
			t.Fatalf("%s: live は自分の台帳だけを読む(Report nil)", c.Key)
		}
	}
	if c := cycles[byKey["c2"]]; !c.Until.Equal(jstDay(2026, 8, 24)) {
		t.Fatalf("live c2 until = %v, want 2026-08-24", c.Until)
	}
	if c := cycles[byKey["c3"]]; !c.Until.Equal(jstDay(2026, 9, 14)) {
		t.Fatalf("live c3 until = %v, want 2026-09-14", c.Until)
	}
	if c := cycles[byKey[All]]; !c.Since.IsZero() || !c.Until.IsZero() {
		t.Fatalf("live all = %+v", c)
	}
}
