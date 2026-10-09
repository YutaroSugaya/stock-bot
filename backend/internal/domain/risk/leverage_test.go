package risk

import "testing"

// 🛑 レバレッジ上限(建玉合計 ≤ 保証金 × 倍率)。既定 1.0 倍 = 現物相当。
//
// 事前コミット: 「実弾フェーズの建玉合計は保証金額の
// 1.0 倍以内から開始する。上げる判断は人間の commit のみ」。
// **これまで機械強制が無く、余力(required_rate 0.33 = レバ 3.0 倍)を使い切れた**。
// 満額 909,090円 で建てると bot の 30% ブレーカーは建値比 **−3.0%** で発火する
// = 構造的に運用不能。
func TestGrossNotionalCap(t *testing.T) {
	cases := []struct {
		name       string
		collateral int
		openGross  int
		newGross   int
		ratio      float64
		wantOK     bool
	}{
		{"既定1.0倍・枠内", 300000, 0, 300000, 1.0, true},
		{"既定1.0倍・1円超過", 300000, 0, 300001, 1.0, false},
		{"既定1.0倍・既存建玉と合算して超過", 300000, 200000, 150000, 1.0, false},
		{"3.0倍に上げれば通る(設定で変えられる)", 300000, 0, 900000, 3.0, true},
		{"3.0倍でも余力超は通さない", 300000, 0, 900001, 3.0, false},
		{"ratio 0 = 無効(既存構成を壊さない)", 300000, 0, 99999999, 0, true},
		// 🛑 保証金が読めない(0)ときに「上限なし」へ倒すと、照会失敗のたびに
		// レバ規律が消える。fail-close。
		{"保証金 0 は fail-close", 0, 0, 1, 1.0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := WithinGrossNotionalCap(c.collateral, c.openGross, c.newGross, c.ratio)
			if got != c.wantOK {
				t.Errorf("WithinGrossNotionalCap(%d,%d,%d,%v)=%v, want %v",
					c.collateral, c.openGross, c.newGross, c.ratio, got, c.wantOK)
			}
		})
	}
}

// gate に配線されていること。定義だけあって呼ばれない防壁は EvaluateHardSafety と
// 同じ dead code になる。
func TestEvaluateSignal_RejectsOverLeverage(t *testing.T) {
	snap := AccountSnapshot{
		CollateralJPY:         300000,
		OpenGrossNotionalJPY:  250000,
		MaxGrossNotionalRatio: 1.0,
	}
	// 新規 100株 × 2500円 = 250,000円 → 合計 500,000 > 300,000
	d := EvaluateSignal(passingSignal(), passingConfig(), snap, passingSummary())
	if d.Allowed {
		t.Fatal("レバ上限を超えたエントリーが通った")
	}
	if d.Reason == "" {
		t.Error("理由が空(なぜ弾かれたか signal_rejections に残らない)")
	}
}

func TestEvaluateSignal_AllowsWithinLeverage(t *testing.T) {
	snap := AccountSnapshot{
		CollateralJPY:         1000000,
		OpenGrossNotionalJPY:  0,
		MaxGrossNotionalRatio: 1.0,
	}
	if d := EvaluateSignal(passingSignal(), passingConfig(), snap, passingSummary()); !d.Allowed {
		t.Fatalf("枠内なのに弾かれた: %s", d.Reason)
	}
}

// 🛑 既定(ratio 0)では**何も変わらない**。research の 200銘柄・全トリガー採用は
// レバ規律の対象外(紙で資本リスクがゼロ、かつサンプルを censoring したくない)。
func TestEvaluateSignal_LeverageDisabledByDefault(t *testing.T) {
	if d := EvaluateSignal(passingSignal(), passingConfig(), AccountSnapshot{}, passingSummary()); !d.Allowed {
		t.Fatalf("ratio 未設定で挙動が変わった: %s", d.Reason)
	}
}
