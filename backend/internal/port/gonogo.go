package port

import (
	"context"
	"errors"
	"time"
)

// ErrGoNoGoRunning は判定が既に実行中(ロックを生きたプロセスが持つ)なので起動しなかったことを表す。
var ErrGoNoGoRunning = errors.New("gonogo is already running")

// 寄り前の銘柄判定(go/no-go)。**表示と記録だけ**で、発注経路は読まない(CLAUDE.md §4)。
// 判定は bot の外のコマンド(cmd/gonogo)が LLM で出してファイルに書き、bot は画面に出すだけ。

const (
	GoNoGoGo      = "go"
	GoNoGoNoGo    = "no_go"
	GoNoGoUnknown = "unknown"

	GoNoGoStatusOK    = "ok"
	GoNoGoStatusError = "error"
)

// GoNoGoSource は判定の出典 1 件。
type GoNoGoSource struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Published string `json:"published,omitempty"` // YYYY-MM-DD
}

// GoNoGoJudgment は LLM の判定(決定論の後検査を通した後の値)。
type GoNoGoJudgment struct {
	Verdict    string         `json:"verdict"`    // go / no_go / unknown
	Category   string         `json:"category"`   // no_go の分類。go / unknown は none のことが多い
	Confidence string         `json:"confidence"` // low / mid / high
	SummaryJA  string         `json:"summary_ja"`
	Sources    []GoNoGoSource `json:"sources"`
}

// GoNoGoRecord は判定ファイル(YYYY-MM-DD.jsonl)の 1 行。append-only で、同じ銘柄の行が複数あれば
// 最後の成功行が正(成功が無ければ最後の行)。
type GoNoGoRecord struct {
	Date     string    `json:"date"`
	Symbol   string    `json:"symbol"`
	JudgedAt time.Time `json:"judged_at"`
	Status   string    `json:"status"` // ok / error
	GoNoGoJudgment
	Error         string   `json:"error,omitempty"`
	Model         string   `json:"model,omitempty"`
	PromptVersion string   `json:"prompt_version"`
	PacketSHA256  string   `json:"packet_sha256"`
	Strategies    []string `json:"strategies"`
	Tracks        []string `json:"tracks"`
	// Trigger はどの起動経路で判定したか(launchd / catchup / manual / button)。古い行は空。
	Trigger string `json:"trigger,omitempty"`
}

// GoNoGoReader は bot が判定を読む口。ファイルが無ければ空(「未判定」)で、bot は落ちない。
type GoNoGoReader interface {
	ForDate(ctx context.Context, date string) (map[string]GoNoGoRecord, error)
}
