// Package advisor は Claude CLI 経由の port.Advisor。LLM は config を書くだけで発注経路には入らない。
package advisor

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
)

// 本文ゼロは外部要因なので cli_error 扱い(parse_error にしない)。
var errEmptyStdout = errors.New("empty stdout")

// JSON を頼むのは「どの世代のモデルが答えたか」を知るためだけ(素のテキストは報告しない)。
type cliEnvelope struct {
	Type       string                     `json:"type"`
	Result     string                     `json:"result"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
}

// ok=false は envelope でなかった場合。呼び手は raw stdout に退避する(記録のための変更が生成を壊さない)。
func splitEnvelope(stdout []byte) (body string, model string, ok bool) {
	trimmed := strings.TrimSpace(string(stdout))
	if !strings.HasPrefix(trimmed, "{") {
		return "", "", false
	}
	var env cliEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return "", "", false
	}
	// result も modelUsage も無いものは envelope とみなさない(JSON を出力した config を誤って剥がさない)。
	if env.Type != "result" && env.Result == "" && len(env.ModelUsage) == 0 {
		return "", "", false
	}
	models := make([]string, 0, len(env.ModelUsage))
	for m := range env.ModelUsage {
		models = append(models, m)
	}
	sort.Strings(models) // map の反復順は不定 — 記録を決定論にする
	return env.Result, strings.Join(models, ","), true
}

var (
	openFenceRE  = regexp.MustCompile(`^\s*` + "```" + `(?:yaml|yml)?\s*\n`)
	closeFenceRE = regexp.MustCompile(`\n\s*` + "```" + `\s*\n*\s*$`)

	// config_id より前は Claude がたまに前置きする散文なので落とす。
	yamlStartRE = regexp.MustCompile(`(?m)^[ \t]*config_id\s*:`)

	// 本物の config だけが持つキー。Claude が併記する preview ブロックはこれで 0 点になり必ず負ける。
	bodyKeyREs = []*regexp.Regexp{
		regexp.MustCompile(`(?m)^[ \t]*strategy_name\s*:`),
		regexp.MustCompile(`(?m)^[ \t]*entry\s*:`),
		regexp.MustCompile(`(?m)^[ \t]*exit\s*:`),
		regexp.MustCompile(`(?m)^[ \t]*risk\s*:`),
		regexp.MustCompile(`(?m)^[ \t]*mode\s*:`),
	}

	// Claude が config 全体の空白を HTML エンティティ化して出すことがあり、go-yaml が
	// "did not find expected key" で落ちる。最初に戻す(後続 regex が実空白を見る必要がある)。
	spaceEntityRE = regexp.MustCompile(`&#0*32;|&#[xX]0*20;|&nbsp;`)

	// 行頭キーのコロン後の空白落ち。行頭キーだけを触るので値の時刻(14:50)は壊さない。
	colonNoSpaceRE = regexp.MustCompile(`(?m)^([ \t]*[A-Za-z_][A-Za-z0-9_]*):([^ \t\r\n])`)

	// 引用値の前でコロンごと落ちた行頭キー(コロンが無いので colonNoSpaceRE では拾えない)。
	keyNoColonQuoteRE = regexp.MustCompile(`(?m)^([ \t]*[A-Za-z_][A-Za-z0-9_]*)"`)

	// RFC3339 の後に付く括弧付き TZ ラベル(`+09:00 (JST)`)。go-yaml の時刻解釈が落ちる。
	tzLabelRE = regexp.MustCompile(`(\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))\s*\([^)\n]*\)`)
)

type ResponseParser struct{}

func (ResponseParser) Parse(stdout []byte) ([]byte, error) {
	if strings.TrimSpace(string(stdout)) == "" {
		return nil, errEmptyStdout
	}
	// sanitize は fence 抽出より先(後続 regex が実空白と修復済みコロンを見る必要がある)。
	s := sanitizeYAML(string(stdout))
	// fence の中身を優先するが空 fence は勝たせない(実 YAML が捨てられる)。
	if m := fenceBlockRE.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[1]) != "" {
		s = m[1]
	} else {
		s = openFenceRE.ReplaceAllString(s, "")
		s = closeFenceRE.ReplaceAllString(s, "")
	}
	s = strings.TrimSpace(selectBody(s))
	if s == "" {
		return nil, errEmptyStdout
	}
	if !yamlStartRE.MatchString(s) {
		return nil, errors.New("no config_id key found in output")
	}
	return []byte(s + "\n"), nil
}

var fenceBlockRE = regexp.MustCompile("(?s)" + "```" + `(?:yaml|yml)?\s*\n(.*?)\n` + "```")

// config_id 行で割り、スキーマキーが最も多いブロックを採る(preview は 0 点で負ける)。
func selectBody(s string) string {
	locs := yamlStartRE.FindAllStringIndex(s, -1)
	if len(locs) <= 1 {
		if len(locs) == 1 {
			return s[locs[0][0]:] // config_id より前の散文を落とす
		}
		return s
	}
	best, bestScore := "", -1
	for i, loc := range locs {
		end := len(s)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		block := s[loc[0]:end]
		score := 0
		for _, re := range bodyKeyREs {
			if re.MatchString(block) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = block, score
		}
	}
	return best
}

// 🛑 適用順は入れ替えない: HTML エンティティ空白 → 行頭キーのコロン修復 → TZ ラベル。
// 空白を戻す前に行頭キーの regex を当てると、実空白が無いのでキーを見つけられない。
func sanitizeYAML(s string) string {
	s = spaceEntityRE.ReplaceAllString(s, " ")
	s = colonNoSpaceRE.ReplaceAllString(s, "$1: $2")
	s = keyNoColonQuoteRE.ReplaceAllString(s, `$1: "`)
	s = tzLabelRE.ReplaceAllString(s, "$1")
	return s
}

type cliOutcome int

const (
	cliOutcomeOK cliOutcome = iota
	cliOutcomeTransient
	cliOutcomeUsageLimit
)

var (
	transientCLIErrorRE = regexp.MustCompile(`(?i)API Error|Overloaded|ConnectionRefused|connection refused|Unable to connect to API`)
	usageLimitRE        = regexp.MustCompile(`(?i)session limit|usage limit|hit your.{0,20}limit`)

	// サーバ側 429(数秒で自己回復する一過性)。🛑 usageLimitRE より**先に**判定すること —
	// 文面 "temporarily limiting requests (not your usage limit) · Rate limited" が
	// "usage limit" を部分文字列として含むので、素朴に usage 判定すると再試行せず諦める。
	serverRateLimitRE = regexp.MustCompile(`(?i)temporarily limiting requests|rate.?limit|not your usage limit|too many requests|\b429\b`)
)

// 🛑 case の順序が意味を持つ(サーバ側 429 を usage 上限より先に見る)。
func classifyCLIOutput(out string) cliOutcome {
	switch {
	case serverRateLimitRE.MatchString(out):
		return cliOutcomeTransient
	case usageLimitRE.MatchString(out):
		return cliOutcomeUsageLimit
	case transientCLIErrorRE.MatchString(out):
		return cliOutcomeTransient
	default:
		return cliOutcomeOK
	}
}

func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return truncate(ln, 200)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// CLIEnvelope / IsUsageLimitOutput / IsTransientOutput は claude CLI の出力の読み方を、
// 同じ CLI を呼ぶ別の adapter(寄り前の go/no-go)と共有する口。分類を写すと片方だけ古くなる。
func CLIEnvelope(stdout []byte) (body, model string, ok bool) { return splitEnvelope(stdout) }

func IsUsageLimitOutput(out string) bool { return classifyCLIOutput(out) == cliOutcomeUsageLimit }

func IsTransientOutput(out string) bool { return classifyCLIOutput(out) == cliOutcomeTransient }
