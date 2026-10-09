package gonogo

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"stockbot/backend/internal/port"
)

// summaryMaxRunes は要約の上限(プロンプトが 200 字以内と頼む。超えたぶんは切る)。
const summaryMaxRunes = 200

var (
	validVerdicts    = map[string]bool{port.GoNoGoGo: true, port.GoNoGoNoGo: true, port.GoNoGoUnknown: true}
	validConfidences = map[string]bool{"low": true, "mid": true, "high": true}
)

// ParseJudgment は LLM の出力(JSON のみを頼む。前置きや ``` があっても最初の { から最後の } を読む)を
// 検査して判定にする。スキーマ違反は error(呼び手が status=error で記録する)。
//
// 🛑 決定論の後検査: **出典が 1 件も無い no_go は unknown に落とす**(根拠の無い no_go を記録に残さない)。
func ParseJudgment(body string) (port.GoNoGoJudgment, error) {
	var j port.GoNoGoJudgment
	start, end := strings.Index(body, "{"), strings.LastIndex(body, "}")
	if start < 0 || end <= start {
		return j, errors.New("schema: JSON が無い")
	}
	dec := json.NewDecoder(strings.NewReader(body[start : end+1]))
	if err := dec.Decode(&j); err != nil {
		return j, fmt.Errorf("schema: %w", err)
	}
	if !validVerdicts[j.Verdict] {
		return j, fmt.Errorf("schema: verdict %q", j.Verdict)
	}
	if !validConfidences[j.Confidence] {
		return j, fmt.Errorf("schema: confidence %q", j.Confidence)
	}
	if j.Category == "" {
		j.Category = "none"
	}
	if _, ok := CategoryLabel[j.Category]; !ok {
		return j, fmt.Errorf("schema: category %q", j.Category)
	}
	j.SummaryJA = strings.TrimSpace(j.SummaryJA)
	if j.SummaryJA == "" {
		return j, errors.New("schema: summary_ja が空")
	}
	if r := []rune(j.SummaryJA); len(r) > summaryMaxRunes {
		j.SummaryJA = string(r[:summaryMaxRunes])
	}
	if j.Sources == nil {
		j.Sources = []port.GoNoGoSource{}
	}
	for _, s := range j.Sources {
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return j, fmt.Errorf("schema: source url %q", s.URL)
		}
	}
	if j.Verdict == port.GoNoGoNoGo && len(j.Sources) == 0 {
		j.Verdict = port.GoNoGoUnknown
		j.SummaryJA = truncateRunes("[出典が無いため no_go → unknown] "+j.SummaryJA, summaryMaxRunes)
	}
	return j, nil
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
