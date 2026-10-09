// Package gonogo は寄り前の銘柄判定(go/no-go)のファイルと claude CLI の adapter。
//
// 🛑 **表示と記録だけ**。bot の発注経路(usecase/command の entry 系・domain/risk)はこの
// package も port.GoNoGoReader も import しない(isolation_test が固定する)。
package gonogo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"stockbot/backend/internal/port"
)

// Journal は判定ファイルの置き場(既定 ~/.stockbot/gonogo)。
//
//   - YYYY-MM-DD.jsonl … 1 判定 1 行の append-only。同じ銘柄は最後の成功行が正。
//   - YYYY-MM-DD.md    … 人が読む形。実行のたびに全体を書き直す(no_go を先頭に)。
type Journal struct {
	Dir string
}

func (j *Journal) Path(date string) string         { return filepath.Join(j.Dir, date+".jsonl") }
func (j *Journal) MarkdownPath(date string) string { return filepath.Join(j.Dir, date+".md") }

// Append は 1 行追記する。1 行は 1 回の write で書く(並行する実行はロックで排他済み)。
func (j *Journal) Append(r port.GoNoGoRecord) error {
	if err := os.MkdirAll(j.Dir, 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(j.Path(r.Date), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// Read はその日の全行を書かれた順に返す。読めない行(書きかけで落ちた最後の行)は飛ばす。
func (j *Journal) Read(date string) ([]port.GoNoGoRecord, error) {
	raw, err := os.ReadFile(j.Path(date))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []port.GoNoGoRecord
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var r port.GoNoGoRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Symbol == "" {
			continue
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// Latest は銘柄ごとに正の 1 行(最後の成功行。成功が無ければ最後の行)。
func Latest(records []port.GoNoGoRecord) map[string]port.GoNoGoRecord {
	out := map[string]port.GoNoGoRecord{}
	for _, r := range records {
		cur, ok := out[r.Symbol]
		if ok && cur.Status == port.GoNoGoStatusOK && r.Status != port.GoNoGoStatusOK {
			continue // 成功の後の失敗は成功を消さない
		}
		out[r.Symbol] = r
	}
	return out
}

// ForDate は port.GoNoGoReader(bot の画面が読む)。
func (j *Journal) ForDate(_ context.Context, date string) (map[string]port.GoNoGoRecord, error) {
	rs, err := j.Read(date)
	if err != nil {
		return nil, err
	}
	return Latest(rs), nil
}

// Succeeded はその日に成功した判定がある銘柄(冪等: これらは判定し直さない)。
func (j *Journal) Succeeded(date string) (map[string]bool, error) {
	rs, err := j.Read(date)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rs {
		if r.Status == port.GoNoGoStatusOK {
			out[r.Symbol] = true
		}
	}
	return out, nil
}

// CategoryLabel は no_go の分類の日本語名(.md と画面)。
var CategoryLabel = map[string]string{
	"accounting_fraud":      "会計不正・不適切会計",
	"audit_opinion":         "監査意見の不表明・限定",
	"earnings_delay":        "決算発表の延期",
	"delisting_risk":        "上場廃止のおそれ(監理・特別注意・整理銘柄)",
	"going_concern":         "継続企業の前提の注記・資金繰り",
	"administrative_action": "行政処分・重大な訴訟",
	"dilution":              "大型の希薄化(公募増資・MSCB など)",
	"other":                 "その他",
	"none":                  "—",
}

var verdictLabel = map[string]string{
	port.GoNoGoNoGo: "NO-GO", port.GoNoGoGo: "GO", port.GoNoGoUnknown: "不明",
}

// mdRank は .md の並び: no_go → 失敗 → 不明 → go。
func mdRank(r port.GoNoGoRecord) int {
	switch {
	case r.Status != port.GoNoGoStatusOK:
		return 1
	case r.Verdict == port.GoNoGoNoGo:
		return 0
	case r.Verdict == port.GoNoGoUnknown:
		return 2
	default:
		return 3
	}
}

// WriteMarkdown はその日の .md を全体ごと書き直す(tmp → rename)。
func (j *Journal) WriteMarkdown(date string) error {
	rs, err := j.Read(date)
	if err != nil {
		return err
	}
	latest := Latest(rs)
	rows := make([]port.GoNoGoRecord, 0, len(latest))
	for _, r := range latest {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(a, b int) bool {
		if ra, rb := mdRank(rows[a]), mdRank(rows[b]); ra != rb {
			return ra < rb
		}
		return rows[a].Symbol < rows[b].Symbol
	})
	var b strings.Builder
	counts := map[string]int{}
	for _, r := range rows {
		if r.Status != port.GoNoGoStatusOK {
			counts["error"]++
		} else {
			counts[r.Verdict]++
		}
	}
	fmt.Fprintf(&b, "# 寄り前の銘柄判定(go/no-go)%s\n\n", date)
	fmt.Fprintf(&b, "NO-GO %d / 失敗 %d / 不明 %d / GO %d(計 %d 銘柄)。**表示と記録だけ** — bot は読まない。止めるなら live タブの「停止」。\n\n",
		counts[port.GoNoGoNoGo], counts["error"], counts[port.GoNoGoUnknown], counts[port.GoNoGoGo], len(rows))
	for _, r := range rows {
		label := verdictLabel[r.Verdict]
		if r.Status != port.GoNoGoStatusOK {
			label = "失敗"
		}
		fmt.Fprintf(&b, "## %s — %s", r.Symbol, label)
		if r.Status == port.GoNoGoStatusOK && r.Category != "" && r.Category != "none" {
			fmt.Fprintf(&b, "(%s)", categoryText(r.Category))
		}
		b.WriteString("\n\n")
		if r.Status != port.GoNoGoStatusOK {
			fmt.Fprintf(&b, "- 失敗: %s(次の起動で判定し直す)\n", r.Error)
		} else {
			fmt.Fprintf(&b, "- 確度: %s\n- 要約: %s\n", r.Confidence, r.SummaryJA)
			for _, s := range r.Sources {
				fmt.Fprintf(&b, "- 出典: [%s](%s) %s\n", nonEmpty(s.Title, s.URL), s.URL, s.Published)
			}
		}
		fmt.Fprintf(&b, "- 戦略: %s / トラック: %s\n- 判定時刻: %s / 起動: %s / %s / packet %s\n\n",
			strings.Join(r.Strategies, ", "), strings.Join(r.Tracks, ", "),
			r.JudgedAt.Format("2006-01-02 15:04:05 MST"), nonEmpty(r.Trigger, "—"), r.PromptVersion, shortHash(r.PacketSHA256))
	}
	tmp := j.MarkdownPath(date) + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, j.MarkdownPath(date))
}

func categoryText(c string) string {
	if l, ok := CategoryLabel[c]; ok {
		return l
	}
	return c
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
