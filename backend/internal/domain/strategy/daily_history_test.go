package strategy

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
)

// 🚨 `high_52w_momentum` は長く**建玉ゼロ**だった。入口が日足 253 本
// (h52Window=252 → evalGuard(…, h52Window+1))を要求するのに、発注経路の供給は
// **裸の 250** が bundle.go の 2 箇所に散らばっていた。`Evaluate` は必ず
// `insufficient_daily_history` を返し、エラーも警告も出ないまま標本がゼロになる。
//
// 数字を上げるだけでは同じ事故が再発する(次に窓の広い戦略を足したら、また裸の数字を
// 直し忘れる)。**供給の本数は「登録戦略の最大要求」に紐づいた 1 つの定数**にして、
// その定数が本当に全戦略を賄えることをここで振る舞いから確かめる。
func armableStrategies() []Strategy {
	return []Strategy{
		BNFReversion{}, BNFReversionTrail{}, HighVolumePremium{}, PostJumpDrift{},
		High52wMomentum{}, AbsMomentumV2{}, ATRBreakoutV2{}, DonchianBreakoutV2{},
		// 兄弟アーム。固定リストのままだと、要求本数の大きいアームを足したときに
		// 「供給が足りているか」の検査から漏れる(52週高値がまさにその形で 4 週間
		// 建玉ゼロだった)。
		AbsMomentumV2Trail{}, ATRBreakoutV2Trail{}, DonchianBreakoutV2Trail{}, High52wMomentumTrail{},
	}
}

// 平坦だが履歴要件だけは満たす系列(入口が成立するかは問わない — 見たいのは
// 「履歴不足で門前払いされないこと」だけ)。
func flatDaily(n int) []market.Candle {
	cs := make([]market.Candle, n)
	for i := range cs {
		p := 1000.0
		cs[i] = market.Candle{Open: p, High: p + 5, Low: p - 5, Close: p, Volume: 1000}
	}
	return cs
}

func TestDailyBarsRequiredCoversEveryArmableStrategy(t *testing.T) {
	d := flatDaily(DailyBarsRequired)
	for _, s := range armableStrategies() {
		sig := s.Evaluate(candIn(s.Name(), d, 1))
		if sig.Reason == "insufficient_daily_history" {
			t.Errorf("%s: 供給 %d 本では履歴不足 — DailyBarsRequired が小さすぎる",
				s.Name(), DailyBarsRequired)
		}
	}
}

// 定数が「最大要求」であって過剰でないこと: 1本減らすと必ずどれかが履歴不足になる。
// これが無いと、要求を下げた戦略を消しても定数だけ大きいまま残る。
func TestDailyBarsRequiredIsTheTightestSufficientSupply(t *testing.T) {
	d := flatDaily(DailyBarsRequired - 1)
	for _, s := range armableStrategies() {
		if s.Evaluate(candIn(s.Name(), d, 1)).Reason == "insufficient_daily_history" {
			return // 期待どおり
		}
	}
	t.Fatalf("供給 %d 本でも全戦略が通る — DailyBarsRequired が過剰",
		DailyBarsRequired-1)
}

// 52週高値が「253 本ちょうど」で門前払いされないことを名指しで固定する
// (この 1 戦略のために定数が存在する)。
func TestHigh52wMomentumEvaluatesAtDailyBarsRequired(t *testing.T) {
	sig := High52wMomentum{}.Evaluate(candIn(config.StrategyHigh52wMomentum, flatDaily(DailyBarsRequired), 1))
	if sig.Reason == "insufficient_daily_history" {
		t.Fatalf("52週高値が %d 本で履歴不足 — 配線バグが再発している", DailyBarsRequired)
	}
	if sig.Reason == "insufficient_breakout_history" {
		t.Fatalf("Donchian の窓に届いていない(%d 本): reason=%q", DailyBarsRequired, sig.Reason)
	}
}
