package pairdiff

import "testing"

// 🚨 事前登録は本命の統計量を
// 「**同一銘柄・同一建値**の trail net − capped net」と書いている。
// 実装は (銘柄, 日) キー + 建値 2% 許容の最近傍へ緩めた
// (コスト床のフラップで trail 脚が遅れて建つとペアが消えるため。理由は妥当)。
//
// 🛑 だが **緩めた事実と、実際にどれだけずれたかを出さないと、事後の裁量になる**。
// 2% は ¥1M 正規化後で約 ¥20,000/¥1M = 測ろうとしている出口効果と同じ桁。
// Pair が建値を 1 つしか持たないと残差を後から出す口が無いので、両脚ぶん残す。
func TestPairKeepsBothEntryPricesSoTheGapIsMeasurable(t *testing.T) {
	trades := []Trade{
		{Symbol: "7203", Strategy: "atr_breakout_v2", EntryPrice: 1000, NetJPY: 500, Day: "2026-08-24"},
		{Symbol: "7203", Strategy: "atr_breakout_v2_trail", EntryPrice: 1010, NetJPY: 800, Day: "2026-08-24"},
	}
	got := MatchWithOpen("atr_breakout_v2", trades, nil)
	if len(got.Pairs) != 1 {
		t.Fatalf("Pairs=%d, want 1", len(got.Pairs))
	}
	p := got.Pairs[0]
	if p.CappedEntry != 1000 {
		t.Errorf("CappedEntry=%v, want 1000", p.CappedEntry)
	}
	if p.TrailEntry != 1010 {
		t.Errorf("TrailEntry=%v, want 1010 — trail 脚の建値を捨てると残差が測れない", p.TrailEntry)
	}
	// 1010 との相対差 = 10/1010 ≈ 0.990%
	if p.EntryGapPct < 0.0098 || p.EntryGapPct > 0.0100 {
		t.Errorf("EntryGapPct=%v, want ≈0.0099", p.EntryGapPct)
	}
}

// 🛑 突合の残差は**集計して開示する**。最大が許容(2%)に張り付いているなら、
// それは「同じトリガー」ではなく別の水準で建った組を数えている疑いになる。
func TestResultReportsTheEntryGapSoItCanBeDisclosed(t *testing.T) {
	trades := []Trade{
		{Symbol: "7203", Strategy: "atr_breakout_v2", EntryPrice: 1000, NetJPY: 100, Day: "2026-08-24"},
		{Symbol: "7203", Strategy: "atr_breakout_v2_trail", EntryPrice: 1000, NetJPY: 200, Day: "2026-08-24"},
		{Symbol: "6758", Strategy: "atr_breakout_v2", EntryPrice: 2000, NetJPY: 100, Day: "2026-08-24"},
		{Symbol: "6758", Strategy: "atr_breakout_v2_trail", EntryPrice: 2020, NetJPY: 200, Day: "2026-08-24"},
	}
	got := MatchWithOpen("atr_breakout_v2", trades, nil)
	if len(got.Pairs) != 2 {
		t.Fatalf("Pairs=%d, want 2", len(got.Pairs))
	}
	// 7203 は 0、6758 は 20/2020 ≈ 0.990%
	if got.MaxEntryGapPct < 0.0098 || got.MaxEntryGapPct > 0.0100 {
		t.Errorf("MaxEntryGapPct=%v, want ≈0.0099", got.MaxEntryGapPct)
	}
	if got.MeanEntryGapPct < 0.0049 || got.MeanEntryGapPct > 0.0050 {
		t.Errorf("MeanEntryGapPct=%v, want ≈0.00495", got.MeanEntryGapPct)
	}
}
