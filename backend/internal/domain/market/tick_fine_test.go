package market

import "testing"

// 期待値は運用データ(backend/data の立花実終値)で裏取り済み — 9432 NTT 151.3/149.2 → 0.1、
// 7203 2921.5/2897.5 → 0.5、8306・4519 は整数円 → 1、6857 28445(5の倍数)→ 5、6920・9983 → 10。
func TestTickSizeOfFineTickSymbols(t *testing.T) {
	cases := []struct {
		symbol string
		price  float64
		want   float64
	}{
		{"9432", 151.3, 0.1},  // ≤1,000
		{"7203", 2921.5, 0.5}, // 1,000超〜3,000
		{"8306", 3685, 1},     // 3,000超〜5,000  (粗いテーブルなら 5)
		{"4519", 7298, 1},     // 5,000超〜10,000 (通常なら 10)
		{"6857", 28445, 5},    // 10,000超〜30,000
		{"6920", 44810, 10},   // 30,000超〜50,000 (通常なら 50)
		{"9983", 75800, 10},   // 50,000超〜100,000 (通常なら 100)
		{"7203", 1000, 0.1},   // 境界: 1,000円「以下」
		{"7203", 1000.5, 0.5}, // 境界: 1,000円「超」
		{"8306", 3000, 0.5},   // 境界: 3,000円「以下」
		{"8306", 3000.5, 1},   // 境界: 3,000円「超」
	}
	for _, c := range cases {
		if got := TickSizeOf(c.symbol, c.price); got != c.want {
			t.Errorf("TickSizeOf(%s, %v) = %v, want %v", c.symbol, c.price, got, c.want)
		}
	}
}

// TOPIX100 以外は従来どおりの通常テーブル。
func TestTickSizeOfOrdinary(t *testing.T) {
	cases := []struct {
		symbol string
		price  float64
		want   float64
	}{
		{"9999", 3900, 5}, // ユニバース外の未知銘柄
		{"9998", 2931, 1},
		{"9997", 900, 1},
		{"6857X", 28445, 10},
	}
	for _, c := range cases {
		if got := TickSizeOf(c.symbol, c.price); got != c.want {
			t.Errorf("TickSizeOf(%s, %v) = %v, want %v (通常テーブル)", c.symbol, c.price, got, c.want)
		}
	}
	// 銘柄不明(空)は粗いテーブル = 刻み違反の注文を作らない側へ倒す。
	if got := TickSizeOf("", 3900); got != 5 {
		t.Errorf("TickSizeOf(\"\", 3900) = %v, want 5", got)
	}
}

// 丸めも銘柄別テーブルに従う(TP/SL は tick の倍数でなければ broker に弾かれる)。
func TestRoundToTickOfUsesSymbolTable(t *testing.T) {
	if got := RoundToTickOf("7203", 2921.3); got != 2921.5 {
		t.Errorf("RoundToTickOf(7203, 2921.3) = %v, want 2921.5 (呼値0.5)", got)
	}
	if got := RoundToTickOf("9999", 3902.4); got != 3900 {
		t.Errorf("RoundToTickOf(未知, 3902.4) = %v, want 3900 (粗いテーブル 呼値5)", got)
	}
	// 3436 (SUMCO) は TOPIX500 なので呼値 1 円 — 丸めても動かない。
	if got := RoundToTickOf("3436", 3902); got != 3902 {
		t.Errorf("RoundToTickOf(3436, 3902) = %v, want 3902 (呼値1)", got)
	}
}

// 生成された銘柄表の健全性。プールとの整合は allowed_symbols を読める internal/config 側の
// TestFineTickTableMatchesAllowedSymbols が digest で照合する。旧ガードは件数(>=200 → ==223)しか見ておらず、
// 表を1件も増やさずにプールを7倍にしても緑のままだった。
func TestFineTickTableIsSane(t *testing.T) {
	if len(fineTickSymbols) == 0 {
		t.Fatal("細かい呼値の表が空 — 全銘柄が粗いテーブルへ倒れる(執行コストが最大10倍で記録される)")
	}
	if FineTickPoolSize == 0 || FineTickPoolDigest == "" {
		t.Fatal("生成物にプールの由来が焼かれていない — 再生成漏れを検出できない")
	}
	// 表は**プールの部分集合**でしかありえない(プールを走査して作るため)。
	if len(fineTickSymbols) > FineTickPoolSize {
		t.Fatalf("細かい呼値 %d 件 > プール %d 件 — 生成が壊れている",
			len(fineTickSymbols), FineTickPoolSize)
	}
	// 代表例(実終値の刻みで裏取り済み・tick_fine_test の先頭コメント参照)。
	for _, sym := range []string{"3436", "6724", "7203", "9432", "8331", "5301"} {
		if !IsFineTickSymbol(sym) {
			t.Errorf("%s が細かい呼値の表に無い", sym)
		}
	}
	// 実価格が整数円しか出ない銘柄を細かい側に載せると実弾で刻み違反注文になる(以前は手書き表が実際にそうだった4件)。
	for _, sym := range []string{"2395", "3865", "8628", "8803"} {
		if IsFineTickSymbol(sym) {
			t.Errorf("%s は実約定が全て整数円(粗い刻み)。細かい側に載せると実弾で刻み違反注文になる", sym)
		}
	}
}

// 生成物と判定規則が一致していること。
func TestFineTickTableAgreesWithInference(t *testing.T) {
	// 0.5 刻みでしか出ない値 → Fine。表に載る銘柄はこの regime を引く。
	if got := InferTickRegime([]float64{2921.5}); got != TickRegimeFine {
		t.Fatalf("InferTickRegime(2921.5) = %v, want Fine", got)
	}
	if TickSizeOf("7203", 2921.5) != TickRegimeFine.TickSize(2921.5) {
		t.Error("表に載る銘柄が Fine の呼値を引いていない")
	}
	if TickSizeOf("8803", 2300) != TickRegimeCoarse.TickSize(2300) {
		t.Error("表に無い銘柄が Coarse の呼値を引いていない")
	}
}
