package risk

import (
	"testing"
)

// 🚨 なぜ「本数」ではなく「銘柄数」の枠が要るか。
//
// 時価取得は 1 リクエストに 120 銘柄まで積め、`BatchQuoteFeed` は 121 銘柄目で
// **間隔をチャンク数だけ伸ばして通信量を一定に保つ**。つまり監視銘柄が増えても
// **API 回数は 1 回も増えず**、代わりに時価の実効間隔が 3秒 → 6秒 → 9秒 と落ちる
// (実測で 6秒へ落ちた)。監視集合は **3 トラック共有**なので、
// paper の建玉が live の OnTick 決済判定と分足の密度まで道連れにする。
//
// 建玉の**本数**は監視集合に効かない(同じ銘柄に 12 アーム乗っても 1 銘柄)。
// 効くのは**銘柄数**だけなので、枠も銘柄数で持つ。
func TestAccountSymbolCap_BlocksOnlyNewSymbols(t *testing.T) {
	t.Run("枠が空いていれば新しい銘柄も通る", func(t *testing.T) {
		snap := AccountSnapshot{AccountOpenSymbols: 79, AccountMaxOpenSymbols: 80}
		if d := EvaluateSignal(passingSignal(), passingConfig(), snap, passingSummary()); !d.Allowed {
			t.Fatalf("rejected: %q", d.Reason)
		}
	})

	t.Run("満杯なら新しい銘柄は落とす", func(t *testing.T) {
		snap := AccountSnapshot{AccountOpenSymbols: 80, AccountMaxOpenSymbols: 80}
		d := EvaluateSignal(passingSignal(), passingConfig(), snap, passingSummary())
		if d.Allowed {
			t.Fatal("満杯なのに新しい銘柄を建てている(監視集合が上限を超える)")
		}
		if d.Reason != "account_open_symbols 80 >= cap 80" {
			t.Fatalf("reason = %q", d.Reason)
		}
	})

	// 🛑 ここが要点: **既に保有している銘柄への建ては枠を消費しない**。監視銘柄が
	// 増えない = API も時価の解像度もコストがゼロなので、止める理由が無い。
	// この 1 行が「少ない銘柄に多くのアームを重ねて N を稼ぐ」方向へのバイアスになる
	// (paper の目的は同時に複数戦略を保持して N を早く貯めること)。
	t.Run("既に保有中の銘柄は満杯でも通る", func(t *testing.T) {
		snap := AccountSnapshot{
			AccountOpenSymbols: 200, AccountMaxOpenSymbols: 80,
			SymbolAlreadyHeld: true,
		}
		if d := EvaluateSignal(passingSignal(), passingConfig(), snap, passingSummary()); !d.Allowed {
			t.Fatalf("既に監視している銘柄を枠で落としてはいけない: %q", d.Reason)
		}
	})

	t.Run("0 は無効", func(t *testing.T) {
		snap := AccountSnapshot{AccountOpenSymbols: 5000, AccountMaxOpenSymbols: 0}
		if d := EvaluateSignal(passingSignal(), passingConfig(), snap, passingSummary()); !d.Allowed {
			t.Fatalf("cap 0 は無効でなければならない: %q", d.Reason)
		}
	})
}

// 入口ごとの枠。全体の銘柄枠を**先着順**で配ると、発火の多い入口(実測で
// donchian + atr が 457 建玉中 322 本 = 70%)が枠を食い尽くし、たまにしか発火しない入口
// (bnf は 3 本)が久しぶりにトリガーしたときに**新しい銘柄を開けない**。
// 入口ごとの枠は「N の平準化」ではなく **銘柄予算の予約**として効く。
func TestEntryArmSymbolCap_ReservesBudgetPerEntry(t *testing.T) {
	t.Run("その入口が満杯なら落とす", func(t *testing.T) {
		snap := AccountSnapshot{EntryArmOpenSymbols: 12, EntryArmMaxOpenSymbols: 12}
		d := EvaluateSignal(passingSignal(), passingConfig(), snap, passingSummary())
		if d.Allowed {
			t.Fatal("入口の枠が満杯なのに建てている")
		}
		if d.Reason != "entry_arm_open_symbols 12 >= cap 12" {
			t.Fatalf("reason = %q", d.Reason)
		}
	})

	// 🛑 **兄弟アームのペアを壊さない**。`X` と `X_trail` は入口が完全に同一で必ず
	// 同じ銘柄に乗るので、片方が入った銘柄はその入口にとって「既に保有中」。枠を
	// 消費させると **2 本目だけが境界で弾かれ、ペア差が構造的に測れなくなる**
	// (「ペア 0 件」になる壊れ方そのもの)。
	t.Run("兄弟が既に持っている銘柄は満杯でも通る", func(t *testing.T) {
		snap := AccountSnapshot{
			EntryArmOpenSymbols: 12, EntryArmMaxOpenSymbols: 12,
			EntryArmHoldsSymbol: true,
		}
		if d := EvaluateSignal(passingSignal(), passingConfig(), snap, passingSummary()); !d.Allowed {
			t.Fatalf("ペアの 2 本目を枠で落としてはいけない: %q", d.Reason)
		}
	})

	t.Run("0 は無効", func(t *testing.T) {
		snap := AccountSnapshot{EntryArmOpenSymbols: 999, EntryArmMaxOpenSymbols: 0}
		if d := EvaluateSignal(passingSignal(), passingConfig(), snap, passingSummary()); !d.Allowed {
			t.Fatalf("cap 0 は無効でなければならない: %q", d.Reason)
		}
	})
}

// 🛑 手動 override(EvaluateHardSafety)では**バイパスされる**枠であること。
// 銘柄枠は資本を守るためのゲートではなく通信・解像度の予算なので、daily_loss /
// emergency / session と同じ「絶対に越えられない」層には置かない。
func TestSymbolCapsAreNotHardSafetyGates(t *testing.T) {
	snap := AccountSnapshot{
		AccountOpenSymbols: 200, AccountMaxOpenSymbols: 80,
		EntryArmOpenSymbols: 99, EntryArmMaxOpenSymbols: 12,
	}
	if d := EvaluateHardSafety(passingSignal(), passingConfig(), snap, passingSummary()); !d.Allowed {
		t.Fatalf("銘柄枠は hard safety ではない(通信予算であって資本の守りではない): %q", d.Reason)
	}
}
