package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"stockbot/backend/internal/config"
)

// 🛑 起動の 1 行に銘柄コードを全部並べない(起動のログが出過ぎる)。
//
// 523 銘柄を並べた "stockbot started" は 1 行で数 KB になり、前景の端末では数十行に折り返されて
// 他の起動ログを押し流していた。銘柄の一覧は日次選定ファイル(`universe: 日次選定ファイルから
// 読み込み` の path)にあるので、ここは本数だけを出す。
func TestLogStarted_PrintsCountsNotTheSymbolList(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	symbols := make([]string, 523)
	for i := range symbols {
		symbols[i] = fmt.Sprintf("%04d", 1000+i)
	}
	logStarted(logger, "paper_config", "paper_live_feed", symbols, 523,
		[]config.StrategyName{"bnf_reversion", "post_jump_drift"}, 10, true)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("1 行の JSON で出ていない: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "stockbot started" {
		t.Fatalf("msg = %v, want stockbot started(db-restore.sh が最終稼働時刻をこの文言で探す)", rec["msg"])
	}
	if _, ok := rec["symbols"]; ok {
		t.Errorf("銘柄コードの一覧を出している(本数だけにする)")
	}
	if rec["universe"] != float64(523) || rec["watched"] != float64(523) {
		t.Errorf("universe / watched = %v / %v, want 523 / 523", rec["universe"], rec["watched"])
	}
	if n := buf.Len(); n > 400 {
		t.Errorf("起動の 1 行が %d バイト(400 以下に収める)", n)
	}
}
