package advisor

import (
	"math"
	"testing"
	"time"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

// dailySeries builds n daily candles ending at `last` close, each with the
// given volume, so the 25-MA and screeners have enough history.
func dailySeries(sym string, n int, base, last, vol float64) []market.Candle {
	cs := make([]market.Candle, 0, n)
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n-1; i++ {
		cs = append(cs, market.Candle{Symbol: sym, OpenTime: t0.AddDate(0, 0, i),
			Open: base, High: base, Low: base, Close: base, Volume: vol})
	}
	cs = append(cs, market.Candle{Symbol: sym, OpenTime: t0.AddDate(0, 0, n-1),
		Open: last, High: last, Low: last, Close: last, Volume: vol})
	return cs
}

func TestBuild_EmptyData_NoOCONoPanic(t *testing.T) {
	p := Build("7203", nil, "", 0)
	if p.Symbol != "7203" || p.Bars != 0 {
		t.Fatalf("unexpected header: %+v", p)
	}
	if p.OCO.TakeProfit != 0 || p.OCO.StopLoss != 0 {
		t.Fatalf("empty data must yield no OCO, got %+v", p.OCO)
	}
	if !p.Advisory {
		t.Fatal("packet must be marked advisory")
	}
}

// A panic-crash long setup: last close is 12%+ below the 25-MA on high volume.
// The OCO must be a BUY, TP toward the 25-MA (above entry), SL 2.0·ATR below
// entry, both tick-aligned, matching the strategy's own frozen geometry exactly.
// **この一致は契約**: bnf.go だけ幾何を変えると cmd/advise の見積りが黙ってズレる。
func TestBuild_BNFLongSetup_OCOMatchesStrategyGeometry(t *testing.T) {
	base, last := 1000.0, 850.0 // last is -15% vs the flat 1000 25-MA
	cs := dailySeries("7203", 40, base, last, 1000)
	// spike the last bar's volume so the panic-volume gate can trigger
	cs[len(cs)-1].Volume = 3000

	p := Build("7203", cs, "2026-07-21", 0)

	if p.Entry != last {
		t.Fatalf("entry should default to last close %.0f, got %.0f", last, p.Entry)
	}
	if p.OCO.Side != string(order.SideBuy) {
		t.Fatalf("BNF long setup must quote a BUY OCO, got %q", p.OCO.Side)
	}
	// authoritative geometry from the strategy package (single source of truth)
	if p.ATR14 <= 0 {
		t.Fatalf("packet は ATR(14) を出すこと(出口幅のスケール): %v", p.ATR14)
	}
	wantTP, wantSL := strategy.BNFReversionExit(p.Entry, p.SMA25, strategy.DefaultBNFStopATR, p.ATR14)
	if math.Abs(p.OCO.TPJPY-wantTP) > 1e-9 || math.Abs(p.OCO.SLJPY-wantSL) > 1e-9 {
		t.Fatalf("OCO ticks diverge from strategy: got tp=%.4f sl=%.4f want tp=%.4f sl=%.4f",
			p.OCO.TPJPY, p.OCO.SLJPY, wantTP, wantSL)
	}
	if p.OCO.TakeProfit <= p.Entry {
		t.Fatalf("long TP %.2f must be above entry %.2f", p.OCO.TakeProfit, p.Entry)
	}
	if p.OCO.StopLoss >= p.Entry {
		t.Fatalf("long SL %.2f must be below entry %.2f", p.OCO.StopLoss, p.Entry)
	}
	if !p.OCO.TPAligned || !p.OCO.SLAligned {
		t.Fatalf("TP/SL must be 呼値-aligned: %+v", p.OCO)
	}
}

// When price sits at/above the 25-MA there is no BNF long target below the MA:
// the tool must decline to quote an OCO rather than emit a nonsensical TP below
// entry.
func TestBuild_PriceAbove25MA_OmitsOCO(t *testing.T) {
	cs := dailySeries("7203", 40, 1000, 1100, 1000) // last above the MA
	p := Build("7203", cs, "2026-07-21", 0)
	if p.OCO.TakeProfit != 0 {
		t.Fatalf("no long setup above the 25-MA — OCO must be omitted, got %+v", p.OCO)
	}
}

func TestBuild_EntryOverride_UsedForOCO(t *testing.T) {
	cs := dailySeries("7203", 40, 1000, 850, 1000)
	p := Build("7203", cs, "2026-07-21", 800) // override below last
	if p.Entry != 800 {
		t.Fatalf("entry override ignored: got %.0f", p.Entry)
	}
}

// ATR が取れない(履歴不足)ときは OCO を見積らない — 固定率に落ちて
// 「ボラに追従しない SL」を人間に手渡す経路を残さない。
func TestBuild_NoATR_OmitsOCO(t *testing.T) {
	// 直近 15 本が完全に同値 = true range 0 → ATR(14)=0。それでも 25MA は
	// 過去の高い水準を引きずるので sma25 > entry(BNF long の形)は成立する。
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cs := make([]market.Candle, 0, 40)
	for i := 0; i < 40; i++ {
		p := 1000.0
		if i >= 25 {
			p = 850.0
		}
		cs = append(cs, market.Candle{Symbol: "7203", OpenTime: t0.AddDate(0, 0, i),
			Open: p, High: p, Low: p, Close: p, Volume: 3000})
	}
	p := Build("7203", cs, "2026-07-21", 0)
	if p.OCO.TakeProfit != 0 || p.OCO.StopLoss != 0 || p.OCO.SLJPY != 0 {
		t.Fatalf("ATR 不明では OCO を見積らない: %+v", p.OCO)
	}
	if !hasNote(p, "atr_unavailable__oco_omitted") {
		t.Fatalf("省いた理由を note に残すこと: %v", p.Notes)
	}
}

func hasNote(p Packet, want string) bool {
	for _, n := range p.Notes {
		if n == want {
			return true
		}
	}
	return false
}
