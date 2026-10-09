package advisor

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// TestSmoke_RealClaudeCLI — **本物の `claude` を1回叩く** end-to-end 確認。
//
// なぜ要るか: 単体テストは合成した JSON envelope で splitEnvelope を検証している
// だけで、実際の CLI が返す envelope で「本文(複数行 YAML)が result から無傷で
// 取り出せるか」は誰も確かめていない。ここが壊れると advisor は毎 run
// parse_error(= fail-closed で arm しない)になり、**取引事故は起きないが
// 収集中の forward データが丸ごと欠ける**。研究モードで一番痛い壊れ方なので、
// CLI 引数や envelope の扱いを変えたら人間がこれを1回回す。
//
// 課金が発生するので既定では skip。実行:
//
//	STOCKBOT_ADVISOR_SMOKE=1 go test ./internal/adapter/advisor/ -run Smoke -v
//
// 判定は「Status が success か」だけ。no_trade を返しても success(= 経路は健全)。
func TestSmoke_RealClaudeCLI(t *testing.T) {
	if os.Getenv("STOCKBOT_ADVISOR_SMOKE") != "1" {
		t.Skip("STOCKBOT_ADVISOR_SMOKE=1 のときだけ実行(本物の claude を叩いて課金される)")
	}
	promptPath := os.Getenv("STOCKBOT_ADVISOR_PROMPT")
	if promptPath == "" {
		promptPath = "../../../../prompts/generate_strategy_config.md"
	}
	if _, err := os.Stat(promptPath); err != nil {
		t.Skipf("prompt が見つからない(%s): %v", promptPath, err)
	}

	// effort / timeout は計測のために上書きできる(既定は本番と同じ引数)。
	if e := os.Getenv("STOCKBOT_ADVISOR_SMOKE_EFFORT"); e != "" {
		orig := claudeModelArgs
		claudeModelArgs = []string{"--model", "opus", "--effort", e}
		defer func() { claudeModelArgs = orig }()
	}
	timeoutSec := 600
	if v := os.Getenv("STOCKBOT_ADVISOR_SMOKE_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			timeoutSec = n
		}
	}
	a := New("claude", promptPath, "../../../..", timeoutSec)
	a.TransientRetries = 0
	started := time.Now()

	now := time.Date(2026, 7, 27, 9, 5, 0, 0, time.UTC)
	summary := &market.MarketSummary{
		Symbol:      "7203",
		GeneratedAt: now,
		TickSize:    0.5,
		CurrentRate: market.CurrentRate{Bid: 2810, Ask: 2810.5, Last: 2810, SpreadTicks: 1, At: now},
	}

	run, err := a.Generate(context.Background(), summary)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	t.Logf("elapsed=%.0fs status=%s model=%q parsed=%dB err=%q",
		time.Since(started).Seconds(), run.Status, run.Model, len(run.ParsedYAML), run.ErrorMsg)

	if run.Status != port.AdvisorRunSuccess {
		t.Fatalf("本物の CLI で success にならない(この状態だと収集が全滅する): status=%s err=%s\n--- 本文 ---\n%s",
			run.Status, run.ErrorMsg, truncateForLog(string(run.OutputYAML)))
	}
	if run.Model == "" {
		t.Fatalf("model が取れていない — envelope の modelUsage を読めていない")
	}
	if len(run.ParsedYAML) == 0 {
		t.Fatal("ParsedYAML が空")
	}
}

func truncateForLog(s string) string {
	if len(s) > 1500 {
		return s[:1500] + "…(truncated)"
	}
	return s
}
