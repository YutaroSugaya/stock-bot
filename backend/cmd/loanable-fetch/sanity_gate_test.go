package main

import (
	"strings"
	"testing"
)

// 🚨 **0 件で空の一覧を提案する穴**。
//
// これが無いと 0 件でも素通りして `✅ 貸借銘柄として出す : 0 銘柄` と空の
// `loanable_symbols:` を印字する。人間がそれを commit すると
// `risk.EvaluateShortLoanable` が `loanable_list_missing` で**全 SELL を fail-close で
// reject** する = **エラーも出さずに売り側の標本が丸ごと欠測する**。
//
// 判定順が根本原因 → 症状なのは意図的で、エラーが「0 件でした」ではなく
// 「規制マスタが届いていない」と言えるようにするため。
func TestSanityGate(t *testing.T) {
	const allowed = 1549
	tests := []struct {
		name                                string
		kiseiRows, halted, missing, symbols int
		wantErr                             bool
		wantWord                            string
	}{
		{"正常", 546, 27, 2, 1450, false, ""},
		{"規制マスタが 0 行(根本原因)", 0, 0, 2, 1450, true, "規制マスタ"},
		{"売建停止が 0(規制の読み方が変わった)", 546, 0, 2, 1450, true, "0"},
		{"マスタ非カバーが 1% 超(前方で切り詰め)", 546, 27, 20, 1450, true, "切り詰め"},
		{"貸借が 1000 未満", 546, 27, 2, 999, true, "1000"},
		{"下限ちょうどは通す", 546, 27, 15, 1000, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := sanityGate(tt.kiseiRows, tt.halted, tt.missing, allowed, tt.symbols)
			if tt.wantErr && err == nil {
				t.Fatal("落とすはずが通った(空の一覧を人間に commit させる経路)")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("通すはずが落ちた: %v", err)
			}
			if tt.wantErr && tt.wantWord != "" && !strings.Contains(err.Error(), tt.wantWord) {
				t.Fatalf("原因が分かるメッセージにする(%q を含めたい): %v", tt.wantWord, err)
			}
		})
	}
}

// 🛑 **判定順は根本原因から。** 全部の条件が同時に成立したとき、返るのは
// 「規制マスタが届いていない」でなければならない(「0 銘柄でした」と言われても
// 人間は原因を診断できない)。
func TestSanityGate_ReportsRootCauseFirst(t *testing.T) {
	err := sanityGate(0, 0, 999, 1549, 0)
	if err == nil {
		t.Fatal("落とすはず")
	}
	if !strings.Contains(err.Error(), "規制マスタ") {
		t.Fatalf("根本原因(規制マスタが 0 行)を先に報告する: %v", err)
	}
}
