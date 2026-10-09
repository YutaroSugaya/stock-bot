package pairdiff

import "testing"

// 🚨 **「片側しか決済トレードが無い」を「片側しか建たなかった」と読んではいけない。**
//
// コスト床の検出器が数えたいのは「コスト床で **建たなかった**」ペアだが、実際に数えて
// いたのは「決済トレード行が片方しか無い」だった。兄弟脚がまだ **OPEN** のペアが
// そこに混ざると:
//
//   - MeanDiff が打ち切り(censoring)で系統的に偏る(決済が早い方だけ拾う)
//   - BrokenByArm がコスト床の検出器として読めなくなる
//
// MaxHold が 63 / 126 営業日の 2 ペアでは影響が支配的。
func TestMatch_SeparatesStillOpenFromNeverEntered(t *testing.T) {
	trades := []Trade{
		{Symbol: "7203", Strategy: "abs_momentum_v2", EntryPrice: 1000, NetJPY: 100, Day: "2026-08-24"},
		{Symbol: "6501", Strategy: "abs_momentum_v2", EntryPrice: 2000, NetJPY: -50, Day: "2026-08-24"},
	}
	// 7203 の trail 脚は**まだ建玉中**(決済トレードがまだ無いだけ)。
	// 6501 の trail 脚は**そもそも建たなかった**(コスト床で落ちた)。
	open := []OpenLeg{{Symbol: "7203", Strategy: "abs_momentum_v2_trail", EntryPrice: 1000, Day: "2026-08-24"}}

	res := MatchWithOpen("abs_momentum_v2", trades, open)

	if len(res.Pairs) != 0 {
		t.Fatalf("pairs = %d, want 0(どちらも成立していない)", len(res.Pairs))
	}
	if len(res.Pending) != 1 || res.Pending[0].Symbol != "7203" {
		t.Fatalf("pending = %+v, want 7203 の 1 件(兄弟脚が建玉中)", res.Pending)
	}
	if len(res.Unmatched) != 1 || res.Unmatched[0].Symbol != "6501" {
		t.Fatalf("unmatched = %+v, want 6501 の 1 件(建たなかった)", res.Unmatched)
	}
	// 🛑 検出器はこちらだけを数える。建玉中を混ぜるとコスト床の証拠にならない。
	if got := res.BrokenByArm()["abs_momentum_v2_trail"]; got != 1 {
		t.Fatalf("BrokenByArm[trail] = %d, want 1", got)
	}
}

// 🚨 建値は経路で下位桁がずれる。コスト床のフラップで trail 脚が日中に遅れて建つと
// 1 呼値ぶん動くことがあり、キーが完全一致だとペアが消えるうえ **両アームに 1 件ずつ**
// 「建たなかった」が計上されて BrokenByArm の比が 1 に寄る。
// 同じ (銘柄, 日) の中では**建値が最も近い者どうし**を組む。
func TestMatch_PairsAcrossASmallEntryPriceDrift(t *testing.T) {
	trades := []Trade{
		{Symbol: "7203", Strategy: "atr_breakout_v2", EntryPrice: 1000.0, NetJPY: 100, Day: "2026-08-24"},
		{Symbol: "7203", Strategy: "atr_breakout_v2_trail", EntryPrice: 1000.5, NetJPY: 250, Day: "2026-08-24"},
	}
	res := MatchWithOpen("atr_breakout_v2", trades, nil)
	if len(res.Pairs) != 1 {
		t.Fatalf("pairs = %d, want 1 — 1 呼値のずれでペアが壊れている(unmatched=%+v)", len(res.Pairs), res.Unmatched)
	}
	if res.Pairs[0].Diff != 150 {
		t.Fatalf("diff = %v, want 150", res.Pairs[0].Diff)
	}
	if len(res.Unmatched) != 0 {
		t.Fatalf("unmatched = %+v, want 0", res.Unmatched)
	}
}

// 建値が桁違いなら別トリガー。無理に組まない。
func TestMatch_DoesNotPairAcrossDifferentTriggers(t *testing.T) {
	trades := []Trade{
		{Symbol: "7203", Strategy: "atr_breakout_v2", EntryPrice: 1000, NetJPY: 100, Day: "2026-08-24"},
		{Symbol: "7203", Strategy: "atr_breakout_v2_trail", EntryPrice: 1500, NetJPY: 250, Day: "2026-08-24"},
	}
	res := MatchWithOpen("atr_breakout_v2", trades, nil)
	if len(res.Pairs) != 0 {
		t.Fatalf("pairs = %+v, want 0(建値が 50%% 違う = 別トリガー)", res.Pairs)
	}
	if len(res.Unmatched) != 2 {
		t.Fatalf("unmatched = %d, want 2", len(res.Unmatched))
	}
}

// 🚨 同じ (銘柄, 建値, 日) で 2 回決済していると、map 代入では 1 本が黙って消えていた。
// 全部を突き合わせ対象に残す。
func TestMatch_KeepsDuplicateTriggersOnTheSameDay(t *testing.T) {
	trades := []Trade{
		{Symbol: "7203", Strategy: "donchian_breakout_v2", EntryPrice: 1000, NetJPY: 100, Day: "2026-08-24"},
		{Symbol: "7203", Strategy: "donchian_breakout_v2", EntryPrice: 1000, NetJPY: -30, Day: "2026-08-24"},
		{Symbol: "7203", Strategy: "donchian_breakout_v2_trail", EntryPrice: 1000, NetJPY: 200, Day: "2026-08-24"},
		{Symbol: "7203", Strategy: "donchian_breakout_v2_trail", EntryPrice: 1000, NetJPY: -10, Day: "2026-08-24"},
	}
	res := MatchWithOpen("donchian_breakout_v2", trades, nil)
	if len(res.Pairs) != 2 {
		t.Fatalf("pairs = %d, want 2 — 同一キーの 2 本目が黙って捨てられている", len(res.Pairs))
	}
	if len(res.Unmatched) != 0 {
		t.Fatalf("unmatched = %+v, want 0", res.Unmatched)
	}
}

// 既存の Match は MatchWithOpen(…, nil) と同じであり続ける(呼び手を壊さない)。
func TestMatch_BackwardCompatible(t *testing.T) {
	trades := []Trade{
		{Symbol: "7203", Strategy: "high_52w_momentum", EntryPrice: 1000, NetJPY: 100, Day: "2026-08-24"},
		{Symbol: "7203", Strategy: "high_52w_momentum_trail", EntryPrice: 1000, NetJPY: 250, Day: "2026-08-24"},
	}
	if a, b := MatchWithOpen("high_52w_momentum", trades, nil), MatchWithOpen("high_52w_momentum", trades, nil); len(a.Pairs) != len(b.Pairs) {
		t.Fatalf("Match と MatchWithOpen が食い違う: %d vs %d", len(a.Pairs), len(b.Pairs))
	}
}
