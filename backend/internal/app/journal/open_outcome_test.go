package journal

import (
	"os"
	"path/filepath"
	"testing"

	"stockbot/backend/internal/usecase/query"
)

// 🚨 **「判断した結果がどうだったか」がパケットに 1 つも入っていなかった**。
//
// open.positions は {symbol, strategy, quantity, entry_price, opened_at, held_days} だけで、
// **含み損益も MFE/MAE も当日終値も無い**。だから段2 は台帳の書き写ししかできず、
// 「DB を見れば分かること」しか書けなかった。
//
// 🛑 **これは後から復元できない情報**である点が肝。台帳が持つ MFE/MAE は
// *走っている極値*であって時系列ではない。「その建玉が 3 日目にどこにいたか」は
// **その日に記録しないと永久に失われる**。日次スナップショットがその時系列を作る。
func writeDaily(t *testing.T, dir, sym string, rows string) {
	t.Helper()
	// 実データと同じ形式(セミコロン区切り・ヘッダ無し): date;open;high;low;close;volume
	if err := os.WriteFile(filepath.Join(dir, sym+"_daily.csv"), []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEnrichOpenPositionsAddsTheDaysOutcome(t *testing.T) {
	dir := t.TempDir()
	// 8/24 終値 7000。BUY 建値 7129 → 129円/株の逆行。
	writeDaily(t, dir, "9101", "2026-08-21;7100;7200;7050;7150;1000\n2026-08-24;7150;7180;6990;7000;1200\n")

	open := []query.JournalOpenView{{
		Symbol: "9101", Side: "BUY", Quantity: 100, EntryPrice: 7129,
		PeakPerShareJPY: 51, TroughPerShareJPY: -139,
	}}
	got := EnrichOpenPositions(open, dir, "2026-08-24")

	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	p := got[0]
	if p.ClosePrice != 7000 {
		t.Fatalf("ClosePrice=%v, want 7000 — 当日終値が入っていない", p.ClosePrice)
	}
	// 🛑 単位を混ぜない。MFE/MAE は**円/株**なので、含みも円/株を主に出す。
	if p.UnrealizedPerShareJPY != -129 {
		t.Fatalf("UnrealizedPerShareJPY=%v, want -129", p.UnrealizedPerShareJPY)
	}
	if p.UnrealizedJPY != -12900 {
		t.Fatalf("UnrealizedJPY=%v, want -12900(円/株 × 株数)", p.UnrealizedJPY)
	}
}

// 🛑 SELL は符号が逆。ここを取り違えると空売りの日記が全部逆さまになる。
func TestEnrichOpenPositionsHandlesShorts(t *testing.T) {
	dir := t.TempDir()
	// 8/24 終値 5400。SELL 建値 5535 → 135円/株の順行(利)。
	writeDaily(t, dir, "4704", "2026-08-21;5500;5600;5450;5550;900\n2026-08-24;5550;5560;5380;5400;1100\n")

	got := EnrichOpenPositions([]query.JournalOpenView{{
		Symbol: "4704", Side: "SELL", Quantity: 100, EntryPrice: 5535,
	}}, dir, "2026-08-24")

	if got[0].UnrealizedPerShareJPY != 135 {
		t.Fatalf("UnrealizedPerShareJPY=%v, want +135 — SELL の符号が逆", got[0].UnrealizedPerShareJPY)
	}
}

// 🛑 **当日の日足がまだ無い日は「取得できず」**。0 で埋めると「含みゼロ」に化ける
// (引け直後に段1 を組むと必ずこの状態になる)。
func TestEnrichOpenPositionsMarksMissingBars(t *testing.T) {
	dir := t.TempDir()
	got := EnrichOpenPositions([]query.JournalOpenView{{
		Symbol: "9999", Side: "BUY", Quantity: 100, EntryPrice: 100,
	}}, dir, "2026-08-24")

	if got[0].ClosePrice != 0 || got[0].UnrealizedJPY != 0 {
		t.Fatalf("日足が無いのに数字が入った: %+v", got[0])
	}
	if got[0].Unavailable == "" {
		t.Fatal("日足が無いことが出ていない — 含みゼロと読める")
	}
}
