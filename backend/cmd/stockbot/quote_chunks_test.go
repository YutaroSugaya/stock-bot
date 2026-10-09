package main

import (
	"testing"

	"stockbot/backend/internal/adapter/broker"
)

// 上限ログの定数と、実際にチャンクを切る broker の定数が食い違うと、起動ログの
// 「chunks=N」が実周期と無関係な数字になる(STATUS #6 は上限の再実測を予定している)。
func TestQuoteBatchSizeMatchesBroker(t *testing.T) {
	if tachibanaQuoteBatchSize != broker.DefaultQuoteBatchSize {
		t.Fatalf("tachibanaQuoteBatchSize=%d, broker.DefaultQuoteBatchSize=%d — 片方だけ直した",
			tachibanaQuoteBatchSize, broker.DefaultQuoteBatchSize)
	}
}
