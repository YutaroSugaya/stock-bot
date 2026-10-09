package main

import (
	"testing"

	"stockbot/backend/internal/usecase/query"
)

// 🛑 判定に流し込んでよいのは**エッジ標本**だけ。ダッシュボードの
// `/api/performance` は **口座ベース**(entry_compensated /
// external_close も戦績に計上)で、同じ形の JSON を返す。取り違えて -in に渡すと、
// 「戦略の出口ではない往復」が入った系列で verdict が出て事前コミットが静かに崩れる。
// 名乗りが違う入力は**読まずに落とす**(fail-close)。
func TestRejectsAccountCountedInput(t *testing.T) {
	if err := checkCounting(string(query.CountingAccount)); err == nil {
		t.Fatal("counting=account を受け入れた — 口座ベースの系列で verdict が出る")
	}
}

// エッジ標本、および `counting` を持たない古い入力(手書きの net.json / 過去の
// forward-report 出力)は通す — 名乗りが無いことを理由に判定できなくしない。
func TestAcceptsEdgeSampleAndLegacyInput(t *testing.T) {
	for _, c := range []string{"", string(query.CountingEdgeSample)} {
		if err := checkCounting(c); err != nil {
			t.Fatalf("counting=%q を拒否した: %v", c, err)
		}
	}
}
