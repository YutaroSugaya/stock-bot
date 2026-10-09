package strategy

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/market"
)

// 研究モードで advisor メニューに追加した戦略のスクリーナー。各 Screen は Evaluate の入口ゲートを厳密にミラーする。
// menu 外は screen しない(戦略カタログの Menu 列・catalog_test が強制)。

func seriesFrom(closes []float64, vol float64) []market.Candle {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cs := make([]market.Candle, len(closes))
	for i, c := range closes {
		cs[i] = market.Candle{OpenTime: t0.AddDate(0, 0, i), Open: c, High: c, Low: c, Close: c, Volume: vol}
	}
	return cs
}

func risingCloses(n int, start, step float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = start + float64(i)*step
	}
	return out
}

func TestScreenHigh52w(t *testing.T) {
	// 上昇トレンドの新値圏(直近252日高値の1%以内 + 200日線上)。
	up := seriesFrom(risingCloses(260, 100, 0.5), 1000)
	c := High52wMomentum{}.Screen("7203", up)
	if !c.Triggered {
		t.Fatalf("52週高値圏でトリガーすべき: %+v", c)
	}
	cl := risingCloses(260, 100, 0.5)
	cl[259] = cl[258] * 0.97 // 高値圏から3%下 = 1%以内でない
	c = High52wMomentum{}.Screen("7203", seriesFrom(cl, 1000))
	if c.Triggered {
		t.Fatalf("高値圏でないのにトリガーしてはいけない: %+v", c)
	}
	if c.Score <= 0 {
		t.Fatalf("near-miss はスコアで順位付けする(0 は不可): %+v", c)
	}
}

func TestScreenDonchian(t *testing.T) {
	// 単調上昇 = 毎バーが直近20日高値の更新。
	up := seriesFrom(risingCloses(30, 100, 0.5), 1000)
	c := DonchianBreakout{}.Screen("7203", up)
	if !c.Triggered {
		t.Fatalf("20日高値ブレイクでトリガーすべき: %+v", c)
	}
	cl := risingCloses(30, 100, 0.5)
	cl[29] = cl[28] - 5 // ブレイク失敗
	c = DonchianBreakout{}.Screen("7203", seriesFrom(cl, 1000))
	if c.Triggered {
		t.Fatalf("ブレイクなしでトリガーしてはいけない: %+v", c)
	}
	// 履歴不足は静かに未トリガー。
	c = DonchianBreakout{}.Screen("7203", seriesFrom(risingCloses(10, 100, 0.5), 1000))
	if c.Triggered || c.Detail != "insufficient_history" {
		t.Fatalf("履歴不足の扱いが違う: %+v", c)
	}
}

func TestScreenATRBreakout(t *testing.T) {
	// ±0.25 で振動(ATR ≈ 0.5)する 101 本の後、+1ATR 超のジャンプ。
	cl := make([]float64, 102)
	for i := range cl {
		if i%2 == 0 {
			cl[i] = 99.75
		} else {
			cl[i] = 100.25
		}
	}
	cl[101] = cl[100] + 2 // ATR≈0.5 の 4 倍のレンジ拡大・100日線(≈100)より上
	c := ATRBreakout{}.Screen("7203", seriesFrom(cl, 1000))
	if !c.Triggered {
		t.Fatalf("+1ATR 超のレンジ拡大でトリガーすべき: %+v", c)
	}
	cl[101] = cl[100] + 0.1 // ATR 未満の微動
	c = ATRBreakout{}.Screen("7203", seriesFrom(cl, 1000))
	if c.Triggered {
		t.Fatalf("レンジ拡大なしでトリガーしてはいけない: %+v", c)
	}
}

func TestScreenAbsMomentum(t *testing.T) {
	// 上昇トレンド = 200日線より上 かつ 6ヶ月前より上。
	up := seriesFrom(risingCloses(210, 100, 0.5), 1000)
	c := AbsMomentum{}.Screen("7203", up)
	if !c.Triggered {
		t.Fatalf("絶対モメンタム成立でトリガーすべき: %+v", c)
	}
	// **契約が反転した**。ここは元々「横ばいから最後だけ下 =
	// 200日線割れ → **トリガーしない**」を固定していた。あれは screener が買い側の式しか
	// 見ていなかった頃の形で、**その buy-only こそが売り標本を構造的にゼロにしていた**
	// (入口の式は元から両向きで書かれているのに、枠が配られないので Evaluate が呼ばれない)。
	// いまは「200日線より下 かつ 6ヶ月前より下」= **売りの入口成立**なので Triggered が正しい。
	flat := make([]float64, 210)
	for i := range flat {
		flat[i] = 100
	}
	flat[209] = 99
	c = AbsMomentum{}.Screen("7203", seriesFrom(flat, 1000))
	if !c.Triggered {
		t.Fatalf("200日線割れ+6ヶ月前割れは**売りの入口**なのでトリガーすべき: %+v", c)
	}
	if c.Score <= 1.0 {
		t.Fatalf("売り側 score が 1 以下 = 枠が買いに独占される: %+v", c)
	}

	// **どちらの向きでも成立していない**ときは従来どおりトリガーしない
	// (「両向きにしたら何でも発火する」ようになっていないことの確認)。
	mixed := make([]float64, 210)
	for i := range mixed {
		mixed[i] = 100
	}
	mixed[209] = 100.5 // 200日線より上だが 6ヶ月前(=100)からはほぼ動いていない…ではなく上
	// 200日線より上 かつ 6ヶ月前より上 = 買い成立してしまうので、片側だけ満たす形にする:
	// 直近だけ下げて 200日線より下、しかし 6ヶ月前より上。
	for i := 0; i < 180; i++ {
		mixed[i] = 90
	}
	for i := 180; i < 210; i++ {
		mixed[i] = 110
	}
	mixed[209] = 100 // 200日線(≒93)より上・6ヶ月前(=90)より上 → 買い成立
	c = AbsMomentum{}.Screen("7203", seriesFrom(mixed, 1000))
	if !c.Triggered {
		t.Fatalf("前提: この系列は買いで成立する: %+v", c)
	}
	// 片側も成立しない系列(完全な横ばい)。
	dead := make([]float64, 210)
	for i := range dead {
		dead[i] = 100
	}
	if c = (AbsMomentum{}).Screen("7203", seriesFrom(dead, 1000)); c.Triggered {
		t.Fatalf("どちらの向きも成立していないのにトリガーした: %+v", c)
	}
}
