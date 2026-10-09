package position

import (
	"testing"

	"stockbot/backend/internal/domain/order"
)

// 1 回あたりの延長上限は保有区分で決まる。command(延長の実行)と query(延長候補の
// 列挙)が**同じ数字**を読むためにここに置く — 二重に書くと片方だけ動いてズレる。
func TestMaxExtendMinutesByHoldingMode(t *testing.T) {
	if got := MaxExtendMinutes(order.HoldingMultiday); got != 30*24*60 {
		t.Errorf("multiday = %d, want 30日", got)
	}
	if got := MaxExtendMinutes(order.HoldingIntraday); got != 720 {
		t.Errorf("intraday = %d, want 720", got)
	}
}
