package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/apiusage"
	"stockbot/backend/internal/domain/clock"
)

// dashboard は立花側の集計と**同じ粒度**(CLMID 単位)を出すこと。総数しか無いと
// 上限を超えたときにどの処理が原因かを特定できない。
func TestAPIUsageViewExposesTachibanaComparableFields(t *testing.T) {
	u := apiusage.Usage{
		WindowStart:         time.Date(2026, 8, 12, 5, 30, 0, 0, clock.JST),
		Total:               7670,
		TotalExcludingLogin: 7662,
		ByCLMID:             map[string]int64{"CLMMfdsGetMarketPrice": 6110, "CLMMfdsGetMarketPriceHistory": 1551},
	}
	got := apiUsageView(u, 1)

	if got["total"] != int64(7670) || got["total_excluding_login"] != int64(7662) {
		t.Errorf("総数が落ちている: %v", got)
	}
	if got["cap"] != dailyTachibanaRequestCap {
		t.Errorf("立花の上限 %v が出ていない", got["cap"])
	}
	if got["window_start"] != "2026-08-12T05:30:00+09:00" {
		t.Errorf("窓の起点=%v — 立花の集計期間(5:30起点)と揃っていない", got["window_start"])
	}
	by, ok := got["by_clmid"].(map[string]int64)
	if !ok || by["CLMMfdsGetMarketPrice"] != 6110 {
		t.Errorf("CLMID 別の内訳が出ていない: %v", got["by_clmid"])
	}
}

// 監視 120銘柄超えは通信量ではなく**解像度**に出る。数字が見えないと
// 「なぜ1分足が粗いのか」を後から説明できない。
func TestAPIUsageViewSurfacesQuoteChunks(t *testing.T) {
	if got := apiUsageView(apiusage.Usage{}, 3)["quote_chunks"]; got != 3 {
		t.Errorf("quote_chunks=%v, want 3", got)
	}
	// 不明(0 や負)でも 1 に倒す — 表示のために panic させない。
	if got := apiUsageView(apiusage.Usage{}, 0)["quote_chunks"]; got != 1 {
		t.Errorf("quote_chunks=%v, want 1", got)
	}
}

// 同じ状態で鳴り続けない / 超えた瞬間と戻った瞬間だけ記録すること。
// (15分おきに同じ警告が出るとログが警告で埋まり、本当の変化が見えなくなる)
func TestWarnOnQuoteChunkGrowthOnlyFiresOnChange(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	lc := loopConfig{priceInterval: 3 * time.Second}
	lastQuoteChunks = 0
	t.Cleanup(func() { lastQuoteChunks = 0 })

	warnOnQuoteChunkGrowth(1, lc, logger) // 既定は静か
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("1チャンクで警告が出ている: %s", buf.String())
	}
	warnOnQuoteChunkGrowth(2, lc, logger)
	first := buf.String()
	if !strings.Contains(first, "level=WARN") {
		t.Fatalf("120銘柄超えで警告が出ていない: %s", first)
	}
	if !strings.Contains(first, "effective_interval=6s") {
		t.Errorf("実効間隔が出ていない(なぜ分足が粗いのか後から説明できない): %s", first)
	}
	warnOnQuoteChunkGrowth(2, lc, logger) // 変化なし
	if buf.String() != first {
		t.Errorf("同じ状態で鳴り続けている: %s", buf.String())
	}
	buf.Reset()
	warnOnQuoteChunkGrowth(1, lc, logger) // 戻った
	if !strings.Contains(buf.String(), "上限内に戻った") {
		t.Errorf("復帰が記録されていない: %s", buf.String())
	}
}
