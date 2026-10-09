package strategy

import (
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/ta"
)

// トレンド系4戦略に **入口が完全に同一で出口だけが違う** 兄弟アーム
// を作る。**v3(置き換え)にはしない** — 置き換えると「固定 TP と トレール のどちらが
// 良いか」という問い自体が測れなくなる。
//
// 構造的な根拠(**損益を見ずに言える**): この 4 戦略はトレンドフォローで、SL に
// Turtle の 2N ストップ(2.0×ATR)を借りているのに、**Turtle が持っていなかった固定 TP
// (3.0×ATR)を後から足している**。トレンドフォローの前提は「損益の大半は少数の特大の
// 勝ちから来る」で、固定 TP は**その特大の勝ちだけを狙い撃ちで切り落とす**。
// 🛑 過去の損益は根拠に使わない。

type trailPair struct {
	base  Strategy
	trail Strategy
	daily func() []market.Candle
}

func trailPairs() []trailPair {
	return []trailPair{
		{AbsMomentumV2{}, AbsMomentumV2Trail{}, absV2FreshCross},
		{ATRBreakoutV2{}, ATRBreakoutV2Trail{}, atrV2Compressed},
		{DonchianBreakoutV2{}, DonchianBreakoutV2Trail{}, dbV2FirstDay},
		// 4 ペア目。`high_52w_momentum_trail` の
		// `Evaluate` は**一度も実行されていなかった**(カバレッジ count=0)。
		// 253 本の日足が要るので専用のフィクスチャを使う。
		{High52wMomentum{}, High52wMomentumTrail{}, h52NearHigh},
	}
}

// h52NearHigh: 52週高値圏かつ 200日線より上(= high_52w の買い入口が成立)。
func h52NearHigh() []market.Candle {
	return steadyUp(DailyBarsRequired + 2)
}

func trailIn(t *testing.T, name config.StrategyName, d []market.Candle) EvalInput {
	t.Helper()
	in := candIn(name, d, 1)
	in.Now = time.Date(2026, 8, 24, 9, 30, 0, 0, maxHoldTZ(t))
	in.Hours = maxHoldHours(t)
	return in
}

// 名前の規則は `bnf_reversion_trail` と同じ(ベース名 + `_trail`)。
func TestTrailArmNamesFollowTheSiblingConvention(t *testing.T) {
	want := map[config.StrategyName]config.StrategyName{
		config.StrategyAbsMomentumV2Trail:      "abs_momentum_v2_trail",
		config.StrategyATRBreakoutV2Trail:      "atr_breakout_v2_trail",
		config.StrategyDonchianBreakoutV2Trail: "donchian_breakout_v2_trail",
		config.StrategyHigh52wMomentumTrail:    "high_52w_momentum_trail",
	}
	for got, name := range want {
		if got != name {
			t.Errorf("名前 = %q, want %q", got, name)
		}
	}
}

// 🛑 **入口は完全に同一**。分岐させると「入口同一」が崩れ、差を出口に帰属できない。
func TestTrailArmEntersExactlyWhenItsSiblingDoes(t *testing.T) {
	for _, p := range trailPairs() {
		d := p.daily()
		b := p.base.Evaluate(trailIn(t, p.base.Name(), d))
		tr := p.trail.Evaluate(trailIn(t, p.trail.Name(), d))
		if b.IsEntry() != tr.IsEntry() {
			t.Fatalf("%s: 入口が一致しない(base=%v/%q trail=%v/%q)",
				p.base.Name(), b.IsEntry(), b.Reason, tr.IsEntry(), tr.Reason)
		}
		if !b.IsEntry() {
			t.Fatalf("%s: 前提として ENTER であること: %q", p.base.Name(), b.Reason)
		}
		if b.Side != tr.Side || b.EntryPrice != tr.EntryPrice {
			t.Fatalf("%s: 側/建値が違う: %v@%v vs %v@%v",
				p.base.Name(), b.Side, b.EntryPrice, tr.Side, tr.EntryPrice)
		}
	}
}

// 幾何表: SL と MaxHold は同じ / TP は無し / ratchet は BNF から借りた 1.0・1.5。
func TestTrailArmGeometryBorrowsTheBNFRatchetAndDropsTheFixedTP(t *testing.T) {
	for _, p := range trailPairs() {
		d := p.daily()
		atr := ta.ATR(d, atrExitPeriod)
		b := p.base.Evaluate(trailIn(t, p.base.Name(), d))
		tr := p.trail.Evaluate(trailIn(t, p.trail.Name(), d))

		if tr.TakeProfitJPY != 0 {
			t.Errorf("%s: TP が付いている(uncapped であること): %v", p.trail.Name(), tr.TakeProfitJPY)
		}
		if tr.StopLossJPY != b.StopLossJPY {
			t.Errorf("%s: SL が兄弟と違う: %v vs %v", p.trail.Name(), tr.StopLossJPY, b.StopLossJPY)
		}
		if tr.MaxHoldMinutes != b.MaxHoldMinutes {
			t.Errorf("%s: MaxHold が兄弟と違う: %d vs %d(変えると変数が 2 つになる)",
				p.trail.Name(), tr.MaxHoldMinutes, b.MaxHoldMinutes)
		}
		if tr.RatchetArmJPY != bnfTrailArmATR*atr || tr.RatchetGivebackJPY != bnfTrailGiveATR*atr {
			t.Errorf("%s: ratchet が BNF の 1.0 / 1.5 ×ATR でない: arm=%v give=%v (atr=%v)",
				p.trail.Name(), tr.RatchetArmJPY, tr.RatchetGivebackJPY, atr)
		}
		if b.RatchetArmJPY != 0 {
			t.Errorf("%s: 兄弟(固定 TP 側)に ratchet が付いた", p.base.Name())
		}
	}
}

// 🚨 `applyCostFloor` は `max(TP, RatchetArm)` を床に当てるので、trail 側の
// 対象は 1.0×ATR = **v2 の 3 倍厳しい**。呼値の粗い / スプレッドの広い銘柄では
// **v2 は建つのに trail は落ちる**。床を緩めて揃えるのは嘘なので、
// **落ちること自体は正しい** — 壊れたペアの件数を数えられるよう理由を固定する。
func TestTrailArmCostFloorIsStricterAndSaysSo(t *testing.T) {
	d := absV2FreshCross()
	// スプレッドを広げて trail 側だけが床に届かない領域を作る。
	spread := 0.0
	for s := 1.0; s < 200; s++ {
		in := candIn(config.StrategyAbsMomentumV2, d, s)
		trIn := candIn(config.StrategyAbsMomentumV2Trail, d, s)
		trIn.Now, in.Now = trailIn(t, "", d).Now, trailIn(t, "", d).Now
		trIn.Hours, in.Hours = maxHoldHours(t), maxHoldHours(t)
		base := AbsMomentumV2{}.Evaluate(in)
		trail := AbsMomentumV2Trail{}.Evaluate(trIn)
		if base.IsEntry() && !trail.IsEntry() {
			spread = s
			if trail.Reason != "tp_below_cost_floor" {
				t.Fatalf("落ちる理由 = %q, want tp_below_cost_floor(件数を数えるキー)", trail.Reason)
			}
			break
		}
	}
	if spread == 0 {
		t.Fatal("trail 側だけが床に落ちる領域が見つからない — 床の非対称が消えている")
	}
}

// メニューは 8 → 12 スクリーナー。`Screen` は **v2 の screener に委譲してラベルだけ
// 張り替える**(独自実装するとスクリーンと入口がズレる)。
func TestTrailArmScreenDelegatesToItsSibling(t *testing.T) {
	for _, p := range trailPairs() {
		d := p.daily()
		bs, ok := p.base.(Screener)
		if !ok {
			t.Fatalf("%s は Screener でない", p.base.Name())
		}
		ts, ok := p.trail.(Screener)
		if !ok {
			t.Fatalf("%s は Screener でない", p.trail.Name())
		}
		b := bs.Screen("7203", d)
		tr := ts.Screen("7203", d)
		if tr.Strategy != p.trail.Name() {
			t.Errorf("ラベルが張り替わっていない: %q", tr.Strategy)
		}
		if tr.Triggered != b.Triggered || tr.Score != b.Score {
			t.Errorf("%s: screen が兄弟と一致しない: trig %v/%v score %v/%v",
				p.trail.Name(), tr.Triggered, b.Triggered, tr.Score, b.Score)
		}
	}
}

// MaxHold は兄弟と同じ(変えるとペアで動く変数が 2 つになる)。メニュー全体を回すので、
// 兄弟を足したら自動で対象に入る。
func TestTrailArmMaxHoldMatchesItsSibling(t *testing.T) {
	n := 0
	for _, name := range MenuNames() {
		base := EntryArmOf(name)
		if base == name {
			continue
		}
		n++
		if got, want := MaxHoldBusinessDays(name), MaxHoldBusinessDays(base); got != want {
			t.Errorf("%s の MaxHold = %d, want %d(兄弟と同じ)", name, got, want)
		}
	}
	if n == 0 {
		t.Fatal("メニューに `_trail` 兄弟アームが 1 本も無い(消えている)")
	}
}
