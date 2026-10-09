package gonogo

import (
	"strings"
	"testing"

	"stockbot/backend/internal/port"
)

func TestParseJudgment_Valid(t *testing.T) {
	body := "結果です\n```json\n" + `{"verdict":"no_go","category":"accounting_fraud","confidence":"high",
 "summary_ja":"不適切会計の疑いで第三者委員会を設置","sources":[{"url":"https://www.release.tdnet.info/x","title":"適時開示","published":"2026-09-29"}]}` + "\n```"
	j, err := ParseJudgment(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if j.Verdict != port.GoNoGoNoGo || j.Category != "accounting_fraud" || len(j.Sources) != 1 {
		t.Fatalf("got %+v", j)
	}
}

// 出典が 1 件も無い no_go は unknown に落とす(決定論の後検査)。
func TestParseJudgment_NoGoWithoutSourcesBecomesUnknown(t *testing.T) {
	j, err := ParseJudgment(`{"verdict":"no_go","category":"other","confidence":"mid","summary_ja":"噂","sources":[]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if j.Verdict != port.GoNoGoUnknown {
		t.Fatalf("出典の無い no_go が残った: %+v", j)
	}
	if !strings.Contains(j.SummaryJA, "出典") {
		t.Errorf("落とした理由が要約に残っていない: %q", j.SummaryJA)
	}
}

func TestParseJudgment_SchemaViolationsAreErrors(t *testing.T) {
	for name, body := range map[string]string{
		"not_json":       "わかりません",
		"bad_verdict":    `{"verdict":"maybe","category":"none","confidence":"low","summary_ja":"x","sources":[]}`,
		"bad_confidence": `{"verdict":"go","category":"none","confidence":"very","summary_ja":"x","sources":[]}`,
		"bad_category":   `{"verdict":"no_go","category":"weather","confidence":"low","summary_ja":"x","sources":[{"url":"https://a.example","title":"t"}]}`,
		"bad_url":        `{"verdict":"go","category":"none","confidence":"low","summary_ja":"x","sources":[{"url":"javascript:alert(1)","title":"t"}]}`,
		"empty_summary":  `{"verdict":"go","category":"none","confidence":"low","summary_ja":"","sources":[]}`,
	} {
		if _, err := ParseJudgment(body); err == nil {
			t.Errorf("%s: スキーマ違反を通した", name)
		}
	}
}

func TestParseJudgment_TruncatesLongSummary(t *testing.T) {
	long := strings.Repeat("あ", 300)
	j, err := ParseJudgment(`{"verdict":"go","category":"none","confidence":"low","summary_ja":"` + long + `","sources":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(j.SummaryJA)); n > 200 {
		t.Fatalf("要約が 200 字を超える: %d", n)
	}
}
