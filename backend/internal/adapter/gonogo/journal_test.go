package gonogo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/port"
)

var _ port.GoNoGoReader = (*Journal)(nil)

func rec(sym, status, verdict string, at time.Time) port.GoNoGoRecord {
	r := port.GoNoGoRecord{Date: "2026-10-02", Symbol: sym, JudgedAt: at, Status: status,
		PromptVersion: "v1", Strategies: []string{"bnf_day2_reversion_trail"}, Tracks: []string{"live"}}
	r.Verdict = verdict
	if status == port.GoNoGoStatusError {
		r.Error = "network"
	}
	return r
}

func TestJournal_AppendAndLatestSuccessWins(t *testing.T) {
	ctx := context.Background()
	j := &Journal{Dir: t.TempDir()}
	t0 := time.Date(2026, 10, 2, 8, 15, 0, 0, time.UTC)
	for _, r := range []port.GoNoGoRecord{
		rec("6594", port.GoNoGoStatusError, "", t0),
		rec("6594", port.GoNoGoStatusOK, port.GoNoGoNoGo, t0.Add(time.Minute)),
		rec("6594", port.GoNoGoStatusError, "", t0.Add(2*time.Minute)), // 成功の後の失敗は成功を消さない
		rec("7203", port.GoNoGoStatusError, "", t0),
	} {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := j.ForDate(ctx, "2026-10-02")
	if err != nil {
		t.Fatal(err)
	}
	if got["6594"].Status != port.GoNoGoStatusOK || got["6594"].Verdict != port.GoNoGoNoGo {
		t.Fatalf("最後の成功行が正: %+v", got["6594"])
	}
	if got["7203"].Status != port.GoNoGoStatusError {
		t.Fatalf("成功が無ければ最後の行: %+v", got["7203"])
	}
	done, _ := j.Succeeded("2026-10-02")
	if !done["6594"] || done["7203"] {
		t.Fatalf("成功済み = %v", done)
	}
}

func TestJournal_MissingFileIsEmpty(t *testing.T) {
	got, err := (&Journal{Dir: t.TempDir()}).ForDate(context.Background(), "2026-10-02")
	if err != nil || len(got) != 0 {
		t.Fatalf("無ければ空: %v %v", got, err)
	}
}

// 書きかけで落ちた最後の行は読み飛ばす(残りの判定まで失わない)。
func TestJournal_SkipsTornLine(t *testing.T) {
	j := &Journal{Dir: t.TempDir()}
	if err := j.Append(rec("6594", port.GoNoGoStatusOK, port.GoNoGoGo, time.Now())); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(j.Path("2026-10-02"), os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(`{"date":"2026-10-02","sym`)
	_ = f.Close()
	got, err := j.ForDate(context.Background(), "2026-10-02")
	if err != nil || got["6594"].Verdict != port.GoNoGoGo {
		t.Fatalf("壊れた行で全部を失った: %v %v", got, err)
	}
}

// 人が読む .md は no_go を先頭に並べ、毎回全体を書き直す。
func TestJournal_MarkdownPutsNoGoFirst(t *testing.T) {
	j := &Journal{Dir: t.TempDir()}
	t0 := time.Date(2026, 10, 2, 8, 15, 0, 0, time.UTC)
	g := rec("1111", port.GoNoGoStatusOK, port.GoNoGoGo, t0)
	n := rec("9999", port.GoNoGoStatusOK, port.GoNoGoNoGo, t0)
	n.Category = "accounting_fraud"
	n.SummaryJA = "第三者委員会の設置を公表"
	n.Sources = []port.GoNoGoSource{{URL: "https://example.com/a", Title: "適時開示"}}
	for _, r := range []port.GoNoGoRecord{g, n, rec("5555", port.GoNoGoStatusError, "", t0)} {
		_ = j.Append(r)
	}
	if err := j.WriteMarkdown("2026-10-02"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(j.Dir, "2026-10-02.md"))
	if err != nil {
		t.Fatal(err)
	}
	md := string(raw)
	if !(strings.Index(md, "9999") < strings.Index(md, "5555") && strings.Index(md, "5555") < strings.Index(md, "1111")) {
		t.Fatalf("並びが no_go → 失敗 → go になっていない:\n%s", md)
	}
	for _, want := range []string{"会計不正", "https://example.com/a", "第三者委員会"} {
		if !strings.Contains(md, want) {
			t.Errorf("%q が無い:\n%s", want, md)
		}
	}
}

// .md の各銘柄に起動経路を出す(場中にボタンで判定した行を、寄り前の判定と見分ける)。
func TestJournal_MarkdownShowsTrigger(t *testing.T) {
	j := &Journal{Dir: t.TempDir()}
	r := port.GoNoGoRecord{Date: "2026-10-05", Symbol: "6594", Status: port.GoNoGoStatusOK, Trigger: "button",
		JudgedAt: time.Date(2026, 10, 5, 10, 12, 0, 0, time.FixedZone("JST", 9*3600))}
	r.Verdict = port.GoNoGoNoGo
	if err := j.Append(r); err != nil {
		t.Fatal(err)
	}
	if err := j.WriteMarkdown("2026-10-05"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(j.MarkdownPath("2026-10-05"))
	if !strings.Contains(string(raw), "起動: button") {
		t.Fatalf("起動経路が無い:\n%s", raw)
	}
}
