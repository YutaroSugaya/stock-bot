package main

import (
	"testing"
	"time"

	"stockbot/backend/internal/adapter/apiusage"
	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/domain/clock"
)

// 🛑 朝の fetch-daily は 1日で最大の塊(現在 1,551回)。bot のプロセス内カウンタには
// 決して現れないので、**ここを同じ永続カウンタへ入れないと 1日の総数は永久に
// 1,551回ぶん過少**になる(立花側の集計と突き合わせると食い違う穴)。
func TestAttachUsageRecorderCountsFetchDailyIntoTheSharedWindow(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 12, 7, 0, 0, 0, clock.JST)
	usage := apiusage.Open(dir, "fetch-daily", func() time.Time { return now })

	tb := broker.NewTachibana("demo", "id", nil, "pw", false, false, nil)
	attachUsageRecorder(tb, usage)

	// bot 側(別プロセス)が同じ窓に積んだぶん。
	other := apiusage.Open(dir, "stockbot", func() time.Time { return now })
	other.Record("CLMMfdsGetMarketPrice")

	usage.Record("CLMMfdsGetMarketPriceHistory") // 実送信の代わり(数え口の配線を見る)
	got := usage.Snapshot()
	if got.Total != 2 {
		t.Errorf("窓の合計=%d, want 2(bot と fetch-daily が合算されていない)", got.Total)
	}
	if got.ByCLMID["CLMMfdsGetMarketPriceHistory"] != 1 {
		t.Errorf("History が数えられていない: %v", got.ByCLMID)
	}
}

// 立花以外のフィード(テスト用の fake など)を渡しても落ちないこと。
func TestAttachUsageRecorderIgnoresNonTachibanaFeeds(t *testing.T) {
	attachUsageRecorder(nil, nil) // panic しない
}
