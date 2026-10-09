package strategy

import (
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
)

func dcandle(close, vol float64, day int) market.Candle {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return market.Candle{
		Symbol: "X", Interval: 24 * time.Hour, OpenTime: base.AddDate(0, 0, day),
		Open: close, High: close, Low: close, Close: close, Volume: vol,
	}
}

// フラット系列。DonchianBreakout だけは等号(last >= hi)で triggered/score 1.0 を返す(Evaluate と同一契約)。
func calm(n int, close, vol float64) []market.Candle {
	cs := make([]market.Candle, n)
	for i := range cs {
		cs[i] = dcandle(close, vol, i)
	}
	return cs
}

func TestScreen_BNF(t *testing.T) {
	// 29 calm bars then a -15% crash on ~2x volume → triggered.
	cs := calm(30, 2000, 1000)
	cs[29] = dcandle(1700, 2000, 29)
	got := BNFReversion{}.Screen("7203", cs)
	if !got.Triggered || got.Score < 1 || got.Strategy != config.StrategyBNFReversion {
		t.Fatalf("crash should trigger BNF: %+v", got)
	}
	// flat tape → not triggered, score below the gate.
	if g := (BNFReversion{}).Screen("7203", calm(30, 2000, 1000)); g.Triggered || g.Score >= 1 {
		t.Fatalf("calm tape must not trigger BNF: %+v", g)
	}
	// deep crash but on LOW volume → not a panic → not triggered.
	cs2 := calm(30, 2000, 1000)
	cs2[29] = dcandle(1700, 300, 29)
	if g := (BNFReversion{}).Screen("7203", cs2); g.Triggered {
		t.Fatalf("low-volume crash must not trigger BNF: %+v", g)
	}
}

func TestScreen_HighVolumePremium(t *testing.T) {
	cs := calm(101, 2000, 1000)
	cs[100] = dcandle(2010, 3000, 100) // new 50d-max volume, >=2x avg, above 100MA
	if g := (HighVolumePremium{}).Screen("6758", cs); !g.Triggered || g.Score < 1 {
		t.Fatalf("high-volume day above trend should trigger GKM: %+v", g)
	}
	if g := (HighVolumePremium{}).Screen("6758", calm(101, 2000, 1000)); g.Triggered {
		t.Fatalf("flat volume must not trigger GKM: %+v", g)
	}
}

func TestScreen_PostJumpDrift(t *testing.T) {
	cs := make([]market.Candle, 62)
	for i := 0; i < 61; i++ {
		px := 2000.0
		if i%2 == 1 {
			px = 2008
		}
		cs[i] = dcandle(px, 1000, i)
	}
	cs[61] = dcandle(cs[60].Close*1.05, 6000, 61) // +5% jump on ~5x volume
	if g := (PostJumpDrift{}).Screen("9984", cs); !g.Triggered || g.Score < 1 {
		t.Fatalf("jump+volume should trigger PEAD: %+v", g)
	}
	// same path with a tiny move → no jump.
	cs[61] = dcandle(cs[60].Close*1.002, 6000, 61)
	if g := (PostJumpDrift{}).Screen("9984", cs); g.Triggered {
		t.Fatalf("small move must not trigger PEAD: %+v", g)
	}
}

func TestRankCandidates_TriggeredFirstDeterministic(t *testing.T) {
	crash := calm(30, 2000, 1000)
	crash[29] = dcandle(1700, 2000, 29) // BNF trigger
	universe := map[string][]market.Candle{
		"7203": crash,
		"6758": calm(30, 2000, 1000), // calm → no trigger
	}
	ranked := RankCandidates(universe, DefaultScreeners())
	if len(ranked) == 0 {
		t.Fatal("expected candidates")
	}
	// 6758(フラット)は donchian の等号で triggered になるが score 1.0 ちょうどで BNF 暴落(≈1.25)を超えない。
	// 首位は 7203 の BNF 暴落。同じ日足ゲートを共有する bnf 家族(bnf / stabilized / intraday とその兄弟)は
	// score が同値で、tiebreak は戦略名の辞書順なので**どのアームが先頭かは固定しない**
	// (bnf_intraday を screener に載せた時点で `bnf_intraday_reversion` が先頭になった)。
	top := ranked[0]
	bnfScore := (BNFReversion{}).Screen("7203", crash).Score
	if top.Symbol != "7203" || !top.Triggered || top.Score != bnfScore {
		t.Fatalf("the triggered BNF crash must rank first, got %+v", top)
	}
	// 同一入力 → 同一順序(Candidate は []Condition を持つので == 比較できない。主要フィールドで比べる)。
	again := RankCandidates(universe, DefaultScreeners())
	for i := range ranked {
		if ranked[i].Symbol != again[i].Symbol || ranked[i].Strategy != again[i].Strategy ||
			ranked[i].Score != again[i].Score || ranked[i].Triggered != again[i].Triggered {
			t.Fatalf("ranking not deterministic at %d: %+v vs %+v", i, ranked[i], again[i])
		}
	}
}

// trail は bnf_reversion と同じ入口の対照アーム。スクリーナーが無いと候補に上がれず標本が永久にゼロになる
// (実測: メニュー9戦略に対しスクリーナーは7つしかなく、trail は1件も選ばれていなかった)。
func TestBNFReversionTrailScreenMatchesBNFReversionGate(t *testing.T) {
	d := calm(30, 2000, 1000)
	d[29] = dcandle(1700, 2000, 29)
	base := BNFReversion{}.Screen("7203", d)
	trail := BNFReversionTrail{}.Screen("7203", d)

	if !base.Triggered {
		t.Fatalf("fixture が BNF 発火していない: %+v", base)
	}
	if trail.Triggered != base.Triggered || trail.Score != base.Score {
		t.Fatalf("入口が一致していない: base=%+v trail=%+v", base, trail)
	}
	if trail.Strategy != config.StrategyBNFReversionTrail {
		t.Fatalf("Strategy = %q, want bnf_reversion_trail", trail.Strategy)
	}
	// 出口が違うことが人間に見えること(同じ行が2本並ぶので区別が要る)。
	if trail.Detail == base.Detail {
		t.Errorf("Detail が同一 — 出口の違いが読めない: %q", trail.Detail)
	}
}
