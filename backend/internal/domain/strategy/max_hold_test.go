package strategy

import (
	"testing"

	"stockbot/backend/internal/config"
)

// 事前登録値。**1つの規則(入口の窓 × 0.5・営業日・切り上げ)**で
// あって6つの自由パラメータではない。窓はすべてコード内の既存定数で、forward データが
// 1本も無かった時点で固定されたもの = 採点結果へのフィットではない。
//
// 🛑 このテーブルを測定中に動かすことは禁止。値を変えるなら新規の検定として
// 登録し直す。リテラルで固定してあるのはそのため。
func TestMaxHoldBusinessDaysMatchesPreRegisteredTable(t *testing.T) {
	cases := []struct {
		name config.StrategyName
		want int
	}{
		{config.StrategyATRBreakoutV2, 7},       // atrN=14
		{config.StrategyDonchianBreakoutV2, 10}, // dbWindow=20
		{config.StrategyHighVolumePremium, 25},  // hvpLookback=50
		{config.StrategyPostJumpDrift, 30},      // peadVolWindow=60
		{config.StrategyAbsMomentumV2, 63},      // absLookback=126
		{config.StrategyHigh52wMomentum, 126},   // h52Window=252
	}
	for _, c := range cases {
		if got := MaxHoldBusinessDays(c.name); got != c.want {
			t.Errorf("MaxHoldBusinessDays(%q) = %d, want %d", c.name, got, c.want)
		}
	}
}

// BNF 3戦略は自前の MaxHold を持つ(「bnf_* に触らない」)。0 = このテーブルは
// 期限を与えない、という意味であって「無期限」を意味しない。
func TestMaxHoldBusinessDaysZeroForStrategiesThatOwnTheirDeadline(t *testing.T) {
	for _, n := range []config.StrategyName{
		config.StrategyBNFReversion,
		config.StrategyBNFReversionTrail,
		config.StrategyBNFIntradayReversion,
		config.StrategyNoTrade,
		"未知の戦略",
	} {
		if got := MaxHoldBusinessDays(n); got != 0 {
			t.Errorf("MaxHoldBusinessDays(%q) = %d, want 0", n, got)
		}
	}
}

// 窓 × 0.5 は**切り上げ**。定数と表がずれていないことを、表のリテラルではなく
// 規則の側から突き合わせる — 定数を動かしたのに表を直し忘れる事故を落とす。
func TestMaxHoldBusinessDaysIsHalfTheEntryWindowRoundedUp(t *testing.T) {
	windows := map[config.StrategyName]int{
		config.StrategyATRBreakoutV2:      atrN,
		config.StrategyDonchianBreakoutV2: dbWindow,
		config.StrategyHighVolumePremium:  hvpLookback,
		config.StrategyPostJumpDrift:      peadVolWindow,
		config.StrategyAbsMomentumV2:      absLookback,
		config.StrategyHigh52wMomentum:    h52Window,
	}
	for name, w := range windows {
		want := (w + 1) / 2 // 切り上げ
		if got := MaxHoldBusinessDays(name); got != want {
			t.Errorf("%q: 窓 %d の半分(切り上げ)は %d だが表は %d", name, w, want, got)
		}
	}
}
