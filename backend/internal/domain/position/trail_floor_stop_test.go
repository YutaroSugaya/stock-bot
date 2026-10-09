package position

import (
	"testing"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

// 板の SL を trail の利確の線まで引き上げるための値段。
// 線 = max(arm, peak − giveback)(RatchetFloorAtArm の建玉)を**価格**に直したもの。
// 線が立っていない(arm 前)・trail でない建玉は ok=false = 引き上げない。
func trailPos() Position {
	return Position{
		Symbol: "5726", Side: order.SideBuy, EntryPrice: 2000, Quantity: 100,
		RatchetArmJPY: 100, RatchetGivebackJPY: 150, RatchetFloorAtArm: true,
	}
}

func TestTrailFloorStopPrice_NotArmedHasNoLine(t *testing.T) {
	p := trailPos()
	p.PeakUnrealizedJPY = 80
	if got, ok := TrailFloorStopPrice(p); ok {
		t.Fatalf("arm 前なのに線を出した: %g", got)
	}
}

func TestTrailFloorStopPrice_HoldsAtArmUntilPeakPassesArmPlusGiveback(t *testing.T) {
	p := trailPos()
	p.PeakUnrealizedJPY, p.RatchetArmed = 180, true
	if got, ok := TrailFloorStopPrice(p); !ok || got != 2100 {
		t.Fatalf("線 = %g(ok=%v), want 2100(建値 +arm)", got, ok)
	}
	// arm 済みの記録が無くても、peak が arm に届いていれば線は立っている(exit.go と同じ判定)。
	p.RatchetArmed = false
	if got, ok := TrailFloorStopPrice(p); !ok || got != 2100 {
		t.Fatalf("peak ≥ arm なのに線が立っていない: %g(ok=%v)", got, ok)
	}
}

func TestTrailFloorStopPrice_TrailsThePeakBeyondArmPlusGiveback(t *testing.T) {
	p := trailPos()
	p.PeakUnrealizedJPY, p.RatchetArmed = 400, true
	if got, ok := TrailFloorStopPrice(p); !ok || got != 2250 {
		t.Fatalf("線 = %g(ok=%v), want 2250(peak 400 − giveback 150)", got, ok)
	}
}

func TestTrailFloorStopPrice_SellLineIsBelowEntry(t *testing.T) {
	p := trailPos()
	p.Side = order.SideSell
	p.PeakUnrealizedJPY, p.RatchetArmed = 400, true
	if got, ok := TrailFloorStopPrice(p); !ok || got != 1750 {
		t.Fatalf("売り建玉の線 = %g(ok=%v), want 1750", got, ok)
	}
}

// capped(TP で降りる・ratchet 無し)は対象外。
func TestTrailFloorStopPrice_CappedHasNoLine(t *testing.T) {
	p := trailPos()
	p.RatchetArmJPY, p.RatchetGivebackJPY = 0, 0
	p.PeakUnrealizedJPY, p.RatchetArmed = 400, true
	if got, ok := TrailFloorStopPrice(p); ok {
		t.Fatalf("capped に線を出した: %g", got)
	}
}

// 値段は呼値に丸める(丸めは domain の 1 か所 = market.RoundToTickOf)。
func TestTrailFloorStopPrice_RoundsToTheTick(t *testing.T) {
	p := trailPos()
	p.EntryPrice, p.RatchetArmJPY = 2418, 101.5
	p.PeakUnrealizedJPY, p.RatchetArmed = 120, true
	want := market.RoundToTickOf("5726", 2418+101.5)
	if got, ok := TrailFloorStopPrice(p); !ok || got != want {
		t.Fatalf("線 = %g(ok=%v), want %g(呼値に丸めた値)", got, ok, want)
	}
}
