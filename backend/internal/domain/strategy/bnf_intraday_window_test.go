package strategy

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
)

// 日中版は **前場(〜11:30)の間に条件が揃ったら買う。揃わなければ見送り**。
// 後場に買うと、14:25 の時間切れで閉じた直後に同じ条件で買い直し、残り 25 分で強制決済される形になる
// (反発を取る時間の無い建玉)。条件そのもの(前日パニック・25 日線未満・5 分足 3 本・反発の陽線)は変えない。
func TestBNFIntraday_BuysOnlyInTheMorningSession(t *testing.T) {
	loc := clock.JST
	for _, c := range []struct {
		name  string
		at    time.Time
		enter bool
	}{
		{"前場の反発は買う(9/14 の 09:25)", time.Date(2026, 9, 14, 9, 25, 0, 0, loc), true},
		{"11:29 はまだ前場", time.Date(2026, 9, 14, 11, 29, 0, 0, loc), true},
		{"11:30 以降は見送り", time.Date(2026, 9, 14, 11, 30, 0, 0, loc), false},
		{"後場の反発は見送り(9/14 の 14:25)", time.Date(2026, 9, 14, 14, 25, 33, 0, loc), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m5 := fiveMin(c.at, 6, true)
			last := m5[len(m5)-1].Close
			for _, sig := range []Signal{
				BNFIntradayReversion{}.Evaluate(bnfiInput(c.at, panicDaily(c.at, 40), m5, last)),
				BNFIntradayReversionTrail{}.Evaluate(bnfiTrailInput(c.at, panicDaily(c.at, 40), m5, last)),
			} {
				if sig.IsEntry() != c.enter {
					t.Fatalf("%s: IsEntry=%v want %v (reason=%q)", sig.StrategyName, sig.IsEntry(), c.enter, sig.Reason)
				}
				if !c.enter && sig.Reason != "outside_morning_entry_window" {
					t.Fatalf("%s: 見送りの理由 = %q, want outside_morning_entry_window", sig.StrategyName, sig.Reason)
				}
			}
		})
	}
}
