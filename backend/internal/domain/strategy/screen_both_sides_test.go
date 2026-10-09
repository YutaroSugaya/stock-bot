package strategy

import (
	"math"
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/ta"
)

// 🚨 **売り側が「エラーも出さずに標本ゼロ」になる欠陥**。
//
// 設計は「4 戦略の入口の式は**既に両向きで書かれている**ので direction を開けるだけ」と
// 言うが、それは `Evaluate` の話。**`Screen` は買い側の式しか `Triggered` にしていなかった**。
// 枠は `Triggered` の候補にしか配られないので、売りの入口が成立している銘柄は
// **一度も arm されず**、`Evaluate` が売りを計算する機会そのものが来ない。
//
// さらに `Score` も買い専用だった(`last/sma` 等)。売り局面では 1 未満になるので、
// 仮に `Triggered` を立てても **per_strategy_n の枠は必ず買い候補に奪われる**。
//
// したがって直すのは 2 点:
//  1. `Triggered` を **入口と同じヘルパ**で判定する(片側でも成立していれば true)
//  2. `Score` を **売り側は同じ式の鏡像**にする。買い側の値は **1 ビットも変えない**
//     (過去の score 分位との比較可能性を壊さないため)

// 下降トレンドの系列: 200日線より下・6ヶ月前より下・20日安値割れ・ATR 超の下方拡大。
func fallingSeries(n int) []market.Candle {
	cs := make([]market.Candle, n)
	for i := 0; i < n; i++ {
		p := 3000 - float64(i)*4
		cs[i] = market.Candle{Open: p, High: p + 4, Low: p - 4, Close: p, Volume: 1000}
	}
	// 最終バーを大きく下へ抜く(ATR 拡大 + チャネル下抜け)。
	n1 := n - 1
	p := cs[n-2].Close - 80
	cs[n1] = market.Candle{Open: cs[n-2].Close, High: cs[n-2].Close, Low: p, Close: p, Volume: 2000}
	return cs
}

func TestScreenersTriggerOnTheShortSideToo(t *testing.T) {
	d := fallingSeries(DailyBarsRequired + 2)
	cases := []struct {
		name string
		s    Screener
	}{
		{"abs_momentum", AbsMomentum{}},
		{"donchian_breakout", DonchianBreakout{}},
		{"atr_breakout", ATRBreakout{}},
		{"high_52w_momentum", High52wMomentum{}},
	}
	for _, c := range cases {
		got := c.s.Screen("7203", d)
		if !got.Triggered {
			t.Errorf("%s: 売り側の入口が成立しているのに Triggered=false — 枠が配られず売り標本は構造的にゼロ(detail=%q)",
				c.name, got.Detail)
		}
	}
}

// 🛑 買い側の score は **1 ビットも変えない**(過去の分位と比較できなくなる)。
func TestScreenerLongScoreIsUnchanged(t *testing.T) {
	d := steadyUp(DailyBarsRequired + 2)
	// 手計算で旧定義を再現して突き合わせる。
	cl := closesOf(d)
	last := cl[len(cl)-1]
	{
		sma := ta.SMA(cl, absTrendSMA)
		past := cl[len(cl)-1-absLookback]
		want := math.Min(last/sma, last/past)
		if got := (AbsMomentum{}).Screen("7203", d).Score; math.Abs(got-want) > 1e-12 {
			t.Errorf("abs_momentum の買い側 score が変わった: %v, want %v", got, want)
		}
	}
	{
		hi, _, _ := ta.Donchian(d[:len(d)-1], dbWindow)
		want := last / hi
		if got := (DonchianBreakout{}).Screen("7203", d).Score; math.Abs(got-want) > 1e-12 {
			t.Errorf("donchian の買い側 score が変わった: %v, want %v", got, want)
		}
	}
}

// 売り側の score は同じ式の**鏡像**。等しい強さの売りシグナルが、等しい強さの買いと
// おおむね同じ score になる = per_strategy_n の枠を買いに独占されない。
func TestScreenerShortScoreIsTheMirrorOfTheLongScore(t *testing.T) {
	d := fallingSeries(DailyBarsRequired + 2)
	for _, c := range []struct {
		name string
		s    Screener
	}{
		{"abs_momentum", AbsMomentum{}},
		{"donchian_breakout", DonchianBreakout{}},
		{"atr_breakout", ATRBreakout{}},
		{"high_52w_momentum", High52wMomentum{}},
	} {
		got := c.s.Screen("7203", d)
		if got.Score <= 1.0 {
			t.Errorf("%s: 売り側 score = %v ≤ 1 — 買い候補に必ず順位で負け、枠が回らない",
				c.name, got.Score)
		}
	}
}

// 画面が嘘をつかない: 売りで Triggered なのに条件が全部「未達」に見える状態を作らない。
func TestScreenerConditionsFollowTheDetectedSide(t *testing.T) {
	d := fallingSeries(DailyBarsRequired + 2)
	got := (AbsMomentum{}).Screen("7203", d)
	if !got.Triggered {
		t.Fatal("前提: 売り側で Triggered であること")
	}
	for _, cond := range got.Conditions {
		if !cond.Met {
			t.Fatalf("Triggered なのに未達の条件が出ている(画面が「なぜ発火したか」を説明できない): %+v", cond)
		}
	}
}

// v2 と兄弟アームにも伝播すること(v2.Screen は v1 に委譲している)。
func TestV2AndTrailScreenersInheritTheShortSide(t *testing.T) {
	d := fallingSeries(DailyBarsRequired + 2)
	for _, s := range []Screener{
		AbsMomentumV2{}, DonchianBreakoutV2{}, ATRBreakoutV2{},
		AbsMomentumV2Trail{}, DonchianBreakoutV2Trail{}, ATRBreakoutV2Trail{},
		High52wMomentumTrail{},
	} {
		got := s.Screen("7203", d)
		// v2 は「初日のみ」等の追加条件があるので Triggered までは要求しない。
		// **score が鏡像になっていること**(= 枠が回りうること)だけを縛る。
		if got.Score <= 1.0 {
			t.Errorf("%s: 売り側 score = %v ≤ 1 — 兄弟に鏡像が伝播していない", got.Strategy, got.Score)
		}
	}
}

// 🛑 買い専用の 2 戦略は**そのまま**(式が片側しか無い・テーゼが片側)。
func TestBuyOnlyScreenersStayBuyOnly(t *testing.T) {
	d := fallingSeries(DailyBarsRequired + 2)
	for _, s := range []Screener{HighVolumePremium{}, PostJumpDrift{}} {
		if got := s.Screen("7203", d); got.Triggered {
			t.Errorf("%s は買い専用のはずが売り局面で Triggered になった", got.Strategy)
		}
	}
	// 入口(Evaluate)も買いを直接渡している = 式が片側しかない。
	for _, s := range []Strategy{HighVolumePremium{}, PostJumpDrift{}} {
		in := candIn(s.Name(), d, 1)
		in.Config.Entry.Direction = config.DirectionBoth
		if sig := s.Evaluate(in); sig.IsEntry() && sig.Side == order.SideSell {
			t.Errorf("%s が売りシグナルを出した(式は片側のはず)", s.Name())
		}
	}
}
