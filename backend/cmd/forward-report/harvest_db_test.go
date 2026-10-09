package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 🚨 **台帳を読む道具は全部 `config.LedgerDSN` を通す**。
//
// 道具ごとに `-db` の switch を写していた頃は、`-db research | live` しか受け付けない道具が
// 残り、手順どおりに `-db harvest` と打つと別サイクルの台帳が黙って出てきた。DB を
// 物理分離した目的が、分離を跨ぐ道具が無いことで裏返る。harvest を含む 3 台帳の解決と
// 未設定時の fail-close は config/ledger_dsn_test.go が縛り、ここは「全道具がそこを通る」を縛る。
// 道具を足したらここに足す。
func TestEveryLedgerToolResolvesTheDBThroughLedgerDSN(t *testing.T) {
	for _, tool := range []string{"forward-report", "counterfactual", "daily-review", "holding-period", "pair-diff"} {
		src, err := os.ReadFile(filepath.Join("..", tool, "main.go"))
		if err != nil {
			// 相対位置が変わったときに黙って skip しない。
			t.Fatalf("%s の main.go が読めない: %v", tool, err)
		}
		if !strings.Contains(string(src), "config.LedgerDSN(") {
			t.Errorf("%s: -db を config.LedgerDSN で解決していない — 台帳の取り違えが静かに通る", tool)
		}
	}
}
