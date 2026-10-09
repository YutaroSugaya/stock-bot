package strategy

import (
	"math"
	"testing"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/ta"
)

// Candidate.StopLossJPY —— **枠を配る前に 1本あたりの計画損失を知るための材料**。
//
// 🚨 なぜ Candidate に載せるか: 上限(risk.AccountSnapshot.MaxRiskPerTradeJPY)は発注前
// ゲートだが、ゲートだけだと **建玉にならない候補が口座の建玉枠を占有し続ける**。
// selector は毎 Tick 同じ日足から決定論的に同じ首位を選ぶので、いつまでも
// 「arm 済み・発注は毎回 reject」のまま枠が空かない —— 8309 が
// 分割未調整で起こした「発注待ちのまま動かない沈黙のデッドロック」と同じ形。
// `Side` を載せた理由(貸借銘柄の売り候補が枠を食う)と完全に同じ構造。
//
// 🛑 **0 は「算出できなかった」であって「損失ゼロ」ではない。** 読み手(selector)は
// 0 を上限判定に使わず素通しし、発注前ゲートに委ねる。

func bnfPanicBars() []market.Candle {
	// 25日線から十分に下、かつ出来高急増で BNF の入口を成立させる系列。
	cs := make([]market.Candle, 0, 60)
	for i := 0; i < 59; i++ {
		p := 1000.0
		cs = append(cs, market.Candle{Open: p, High: p + 10, Low: p - 10, Close: p, Volume: 1000})
	}
	// 最終バーだけパニック急落 + 出来高 3 倍。
	cs = append(cs, market.Candle{Open: 900, High: 905, Low: 840, Close: 850, Volume: 3000})
	return cs
}

func TestBNFReversionScreen_ReportsPlannedStopLossJPY(t *testing.T) {
	d := bnfPanicBars()
	c := BNFReversion{}.Screen("TEST", d)
	if !c.Triggered {
		t.Fatalf("前提が崩れている(入口が成立していない): %s", c.Detail)
	}
	if c.StopLossJPY <= 0 {
		t.Fatal("計画 SL 幅が載っていない — selector が枠を配る前に重さを判定できない")
	}

	// Evaluate 側と**同じ幾何**であること。片方だけ動くと、arm では通したのに
	// 発注前ゲートが必ず落とす(またはその逆の)銘柄が静かに生まれる。
	last := d[len(d)-1]
	_, wantSL := BNFReversionExit(last.Close, ta.SMA(closesOf(d), BNFSMAPeriod),
		DefaultBNFStopATR, ta.ATR(d, BNFATRPeriod))
	if math.Abs(c.StopLossJPY-wantSL) > 1e-9 {
		t.Fatalf("Screen の計画 SL %v が Evaluate の幾何 %v と一致しない", c.StopLossJPY, wantSL)
	}
}

// 入口が成立していない候補にも載せてよいが、**材料が無ければ 0 のまま**。
// 0 を「損失ゼロ」と読ませないための境界。
func TestBNFReversionScreen_LeavesStopZeroWithoutHistory(t *testing.T) {
	short := bnfPanicBars()[:10]
	if c := (BNFReversion{}).Screen("TEST", short); c.StopLossJPY != 0 {
		t.Fatalf("履歴不足なのに計画 SL を出した: %v", c.StopLossJPY)
	}
}

// 🛑 `_trail` 兄弟は入口も screener も同一。**出口だけが違う**ので、
// 基のアームの計画 SL をそのまま引き継ぐ(トレールの初期ストップは同じ 2.0×ATR)。
func TestBNFReversionTrailScreen_InheritsPlannedStop(t *testing.T) {
	d := bnfPanicBars()
	base := BNFReversion{}.Screen("TEST", d)
	trail := BNFReversionTrail{}.Screen("TEST", d)
	if trail.StopLossJPY != base.StopLossJPY {
		t.Fatalf("兄弟アームで計画 SL がズレた: base %v / trail %v", base.StopLossJPY, trail.StopLossJPY)
	}
}
