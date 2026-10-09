package market

import (
	"math"
	"testing"
)

// 所属は観測価格から証拠ベースで決める。Fine は積極的証拠があるときだけで、無ければ Coarse へ倒す。
// 非対称なのは誤りの向きを固定するため: Fine を Coarse と誤る = 執行コストの過大計上(研究上のバイアス・
// 実弾では無害)、Coarse を Fine と誤る = 粗いグリッドに乗らない TP/SL を作り broker に弾かれる。
func TestInferTickRegimeRequiresPositiveEvidence(t *testing.T) {
	cases := []struct {
		name   string
		prices []float64
		want   TickRegime
	}{
		{"観測ゼロ = 証拠なし → Coarse", nil, TickRegimeCoarse},
		{"空スライス = 証拠なし → Coarse", []float64{}, TickRegimeCoarse},
		// 3,000超〜5,000 の粗い刻みは 5。全部が5の倍数なら証拠にならない。
		{"全部が粗いグリッド上 → Coarse", []float64{3685, 3690, 4000, 4995}, TickRegimeCoarse},
		// 3599 は 5 の倍数でない = 粗いテーブルでは約定しえない。
		{"1本でも外れれば → Fine", []float64{3685, 3690, 3599}, TickRegimeFine},
		// 1,000円以下の粗い刻みは 1。小数が出た時点で細かいテーブル。
		{"小数の約定値 → Fine", []float64{885, 886, 885.8}, TickRegimeFine},
		// 帯をまたいでも「その価格における粗い刻み」で判定する。
		{"帯をまたぐ観測(2,900帯の0.5刻み)→ Fine", []float64{3100, 2921.5}, TickRegimeFine},
		{"帯をまたぐが全部グリッド上 → Coarse", []float64{3100, 2921, 12000}, TickRegimeCoarse},
		// 0/負値はデータ欠損。これ自体を証拠にしない。
		{"0や負値は無視する", []float64{0, -1, 3685}, TickRegimeCoarse},
		{"欠損に紛れた証拠は拾う", []float64{0, 3599}, TickRegimeFine},
	}
	for _, c := range cases {
		if got := InferTickRegime(c.prices); got != c.want {
			t.Errorf("%s: InferTickRegime(%v) = %v, want %v", c.name, c.prices, got, c.want)
		}
	}
}

// float の丸め誤差を「グリッド外れ」と誤読すると、粗い銘柄を Fine に昇格させて弾かれる注文を作る。
func TestInferTickRegimeToleratesFloatNoise(t *testing.T) {
	noisy := []float64{3000.0000000001, 2999.9999999999, 12000.000000001}
	if got := InferTickRegime(noisy); got != TickRegimeCoarse {
		t.Errorf("InferTickRegime(丸め誤差だけ) = %v, want Coarse — 誤差を証拠と読むと実弾で刻み違反注文になる", got)
	}
}

// 観測順に依存しない(生成物が実行ごとに変わると台帳の再現性が壊れる)。
func TestInferTickRegimeIsOrderIndependent(t *testing.T) {
	a := InferTickRegime([]float64{3685, 3599, 3690})
	b := InferTickRegime([]float64{3690, 3685, 3599})
	c := InferTickRegime([]float64{3599, 3690, 3685})
	if a != b || b != c {
		t.Errorf("順序で結果が変わる: %v / %v / %v", a, b, c)
	}
}

// 判定した regime と実際に引かれる呼値が一致すること(ずれると静かな不整合になる)。
func TestTickRegimeMatchesTickSizeOf(t *testing.T) {
	if TickRegimeFine.TickSize(2921.5) != 0.5 {
		t.Errorf("Fine.TickSize(2921.5) = %v, want 0.5", TickRegimeFine.TickSize(2921.5))
	}
	if TickRegimeCoarse.TickSize(3685) != 5 {
		t.Errorf("Coarse.TickSize(3685) = %v, want 5", TickRegimeCoarse.TickSize(3685))
	}
	// 収録銘柄は Fine と同じ呼値を返す(7203 は表にある)。
	if got, want := TickSizeOf("7203", 2921.5), TickRegimeFine.TickSize(2921.5); got != want {
		t.Errorf("TickSizeOf(7203) = %v, TickRegimeFine = %v — 一致すべき", got, want)
	}
	// 未収録は Coarse と同じ。
	if got, want := TickSizeOf("9999", 3685), TickRegimeCoarse.TickSize(3685); got != want {
		t.Errorf("TickSizeOf(未知) = %v, TickRegimeCoarse = %v — 一致すべき", got, want)
	}
}

// 🚨 分割調整の痕跡を「細かい呼値の証拠」と読まない。
//
// `fetch-daily` は `ChainLinkSplits` **適用後**の値を CSV に書く(cmd/fetch-daily/main.go)。
// 1:3 分割なら過去バーは 1/3 され、5440 が 1813.3333… になる。粗い刻みに乗らないので
// `InferTickRegime` はこれを Fine の決定的証拠と読むが、**生の約定値ではない**ので
// 前提(粗い銘柄はその刻みの倍数でしか約定しない)が成り立たない。
//
// 実測: 生成テーブルの fine 585 銘柄のうち **93 銘柄**が「ある日を境に off-grid が
// ぱたりと止まり、以後 20〜227 本クリーン」という形をしていた(権利落ち日に集中)。
// 本物の fine 銘柄(7203 / 8306)は最終バーまで密度 0.90 で off-grid が出続ける。
// 誤って Fine にすると、1円刻みの銘柄に 0.5 刻みの TP/SL を作って **broker に注文ごと
// 拒否され、約定済みの建玉が裸で残る**(CLAUDE.md の呼値 🚨 と同じ結末・原因は別)。
func TestInferTickRegimeFromBars_IgnoresStaleSplitAdjustedEvidence(t *testing.T) {
	// 8011 の実データの形: 1/3 調整された過去バー → 分割日から整数。
	adjusted := barsOf(t,
		[]float64{1693.3333333333333, 1756.6666666666665, 1693.3333333333333, 1756.6666666666665},
		[]float64{1766.6666666666665, 1783.3333333333333, 1756.6666666666665, 1773.3333333333333},
	)
	clean := make([][]float64, 0, staleTickEvidenceBars)
	for i := 0; i < staleTickEvidenceBars; i++ {
		clean = append(clean, []float64{1608, 1752, 1608, 1688})
	}
	series := append(adjusted, barsOf(t, clean...)...)

	if got := InferTickRegimeFromBars(series); got != TickRegimeCoarse {
		t.Errorf("分割調整の痕跡 + クリーンな末尾 %d 本 = %v, want Coarse — "+
			"調整済みの値を証拠にすると粗い銘柄に格子外の守りを出して broker に拒否される",
			staleTickEvidenceBars, got)
	}
	// 素の InferTickRegime は「約定値だけ」を見る純粋関数のままにしておく
	// (順序非依存の契約は TestInferTickRegimeIsOrderIndependent が固定している)。
	// バー列を持つ呼び手だけが上の時系列判定を使う。
}

// 本物の fine 銘柄は最終バーまで off-grid を出し続ける(7203 の形)。落としてはいけない。
func TestInferTickRegimeFromBars_KeepsGenuineFineEvidence(t *testing.T) {
	var series [][]float64
	for i := 0; i < staleTickEvidenceBars*3; i++ {
		series = append(series, []float64{3133, 3149, 3121, 3133}) // 3,000超は粗い刻み 5・細かい 1
	}
	if got := InferTickRegimeFromBars(barsOf(t, series...)); got != TickRegimeFine {
		t.Errorf("最終バーまで off-grid が続く系列 = %v, want Fine", got)
	}
}

// 証拠の直後に短いクリーン連続があるだけでは落とさない(本物の fine でも数本は整数で終わる)。
func TestInferTickRegimeFromBars_ToleratesShortCleanTail(t *testing.T) {
	series := [][]float64{{2921.5, 2925, 2918, 2920}}
	for i := 0; i < staleTickEvidenceBars-1; i++ {
		series = append(series, []float64{2920, 2925, 2915, 2920})
	}
	if got := InferTickRegimeFromBars(barsOf(t, series...)); got != TickRegimeFine {
		t.Errorf("クリーンな末尾 %d 本(閾値 %d 未満)= %v, want Fine — 過剰に粗い側へ倒すと"+
			"本物の fine 銘柄の刻みが粗くなり、板に乗らない値段を作る", staleTickEvidenceBars-1, staleTickEvidenceBars, got)
	}
}

// 証拠が 1 本も無ければ Coarse(fail-safe)。
func TestInferTickRegimeFromBars_NoEvidenceIsCoarse(t *testing.T) {
	if got := InferTickRegimeFromBars(barsOf(t, []float64{1000, 1010, 990, 1005})); got != TickRegimeCoarse {
		t.Errorf("証拠なし = %v, want Coarse", got)
	}
}

func barsOf(t *testing.T, ohlc ...[]float64) []Candle {
	t.Helper()
	out := make([]Candle, 0, len(ohlc))
	for _, v := range ohlc {
		if len(v) != 4 {
			t.Fatalf("四本値は 4 つ: %v", v)
		}
		out = append(out, Candle{Open: v[0], High: v[1], Low: v[2], Close: v[3]})
	}
	return out
}

// 🚨 **直近の分割**は鮮度では落ちない(継ぎ目の後が短いので)。再生成で
// 8011 / 9279 / 9900 が実際にこれで昇格した(分割は 4 営業日前)。
// 調整値は「元の値 ÷ 単純な分割比」なので、比を掛け戻すと粗いグリッドに戻る —
// 本物の細かい呼値の値にはこの性質が無い。それを直接の反証に使う。
func TestInferTickRegimeFromBars_IgnoresRecentSplitAdjustedEvidence(t *testing.T) {
	// 8011 の実データ: 1/3 調整(5440 → 1813.3333…)→ 分割日から整数、以後 4 本だけ。
	series := [][]float64{
		{1693.3333333333333, 1756.6666666666665, 1693.3333333333333, 1756.6666666666665},
		{1766.6666666666665, 1783.3333333333333, 1756.6666666666665, 1773.3333333333333},
		{1790, 1833.3333333333333, 1783.3333333333333, 1803.3333333333333},
		{1608, 1752, 1608, 1688},
		{1601, 1602, 1530, 1530},
		{1511, 1521, 1480, 1481},
		{1520, 1530, 1500, 1505},
	}
	if got := InferTickRegimeFromBars(barsOf(t, series...)); got != TickRegimeCoarse {
		t.Errorf("分割 4 営業日後の系列 = %v, want Coarse — 鮮度だけでは落ちないので"+
			"「比を掛け戻すと粗いグリッドに戻る」を反証に使う(8011 で実際に昇格した)", got)
	}
}

// 1/2 調整は値が x.5 になり、細かい呼値(0.5 刻み)の本物と**統計的に区別できない**。
// 落とせるのは「off-grid が継ぎ目で止まっている」という構造だけ。9900 の形。
func TestInferTickRegimeFromBars_IgnoresRecentHalvedEvidence(t *testing.T) {
	series := [][]float64{
		{1089, 1109, 1065.5, 1074},
		{1085, 1088, 1072.5, 1084.5},
		{1080.5, 1092, 1069, 1076},
		{1086, 1138, 1075, 1135},
		{1128, 1186, 1127, 1163},
		{1151, 1162, 1113, 1118},
		{1160, 1170, 1150, 1155},
	}
	if got := InferTickRegimeFromBars(barsOf(t, series...)); got != TickRegimeCoarse {
		t.Errorf("1/2 調整の痕跡 + クリーンな末尾 = %v, want Coarse(9900 で実際に昇格した)", got)
	}
}

// 🛑 本物の細かい呼値を「1/2 調整に見えるから」と落とさない。x.5 が**最後まで**出続ける
// なら継ぎ目が無いので調整ではない。落とすと呼値が粗くなり、紙執行の往復コストが
// 過大に記録される(6 倍で記録された事故と同じ向きの歪み)。
func TestInferTickRegimeFromBars_KeepsGenuineHalfTickSeries(t *testing.T) {
	var series [][]float64
	for i := 0; i < staleTickEvidenceBars*2; i++ {
		series = append(series, []float64{1200.5, 1205, 1198.5, 1202.5})
	}
	if got := InferTickRegimeFromBars(barsOf(t, series...)); got != TickRegimeFine {
		t.Errorf("0.5 刻みが最後まで続く系列 = %v, want Fine", got)
	}
}

// 呼値表の包含関係 = 誤りの向きを固定する根拠。粗い刻みが細かい刻みの整数倍でなければ、
// 「粗い側へ倒せば必ず板に乗る」という fail-safe の前提そのものが崩れる。
func TestCoarseTickIsAlwaysAMultipleOfFineTick(t *testing.T) {
	for _, p := range []float64{
		1, 500, 1000, 1000.5, 2999, 3000, 3001, 5000, 5001, 10000, 10001, 30000,
		30001, 50000, 50001, 100000, 300000, 500000, 1000000, 3000000, 5000000,
		10000000, 30000000, 60000000,
	} {
		c, f := TickSize(p), fineTickSize(p)
		if f <= 0 || c <= 0 {
			t.Fatalf("price %v: 刻みが非正 (coarse=%v fine=%v)", p, c, f)
		}
		if q := c / f; math.Abs(q-math.Round(q)) > 1e-9 {
			t.Errorf("price %v: 粗い刻み %v が細かい刻み %v の整数倍ではない — "+
				"粗い側へ倒す fail-safe が成り立たなくなる", p, c, f)
		}
	}
}
