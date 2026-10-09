package market

import (
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
)

func seamDay(y int, m time.Month, d int, c float64) Candle {
	return Candle{OpenTime: time.Date(y, m, d, 15, 0, 0, 0, clock.JST), Open: c, High: c, Low: c, Close: c, Volume: 1}
}

// 立花の日足は分割未調整。調整済み履歴に貼ると分割日で断裂する(1:5 分割 = 見かけ
// -80% の日次リターン)。実際に 35 件の断裂が混入した。
func TestSeamBreak_RejectsDiscontinuityAtTheSeam(t *testing.T) {
	stored := []Candle{seamDay(2026, 6, 19, 500), seamDay(2026, 6, 22, 496)}
	fetched := []Candle{seamDay(2026, 6, 23, 2854)} // 5.75x
	if got := SeamBreak(stored, fetched); !strings.Contains(got, "断裂") {
		t.Fatalf("分割相当の断裂を通した: %q", got)
	}
}

func TestSeamBreak_AcceptsNormalMove(t *testing.T) {
	stored := []Candle{seamDay(2026, 6, 22, 500)}
	if got := SeamBreak(stored, []Candle{seamDay(2026, 6, 23, 515)}); got != "" {
		t.Fatalf("通常の値動きを拒否した: %q", got)
	}
}

// 断裂が「継ぎ目」ではなく重なりの中に隠れているケース(未調整 vs 調整済)。
func TestSeamBreak_RejectsSameDayLevelMismatchInOverlap(t *testing.T) {
	stored := []Candle{seamDay(2026, 6, 19, 100), seamDay(2026, 6, 22, 101)}
	fetched := []Candle{seamDay(2026, 6, 22, 505), seamDay(2026, 6, 23, 510)} // 同日が 5 倍
	if got := SeamBreak(stored, fetched); !strings.Contains(got, "水準") {
		t.Fatalf("重なり区間の水準ズレを検出できていない: %q", got)
	}
}

func TestSeamBreak_NewSymbolPasses(t *testing.T) {
	if got := SeamBreak(nil, []Candle{seamDay(2026, 6, 23, 100)}); got != "" {
		t.Fatalf("新規銘柄が通らない: %q", got)
	}
}

// 既存履歴の内部にある実相場の暴落(値幅制限いっぱい)はローカル日足に 33 件実在し、
// これを弾くと BNF が最も狙いたい銘柄の日足が二度と更新されなくなる。
func TestSeamBreak_IgnoresOldRealCrashInHistory(t *testing.T) {
	stored := []Candle{
		seamDay(2011, 3, 14, 1000), seamDay(2011, 3, 15, 700), // 震災翌日 -30%(実相場)
		seamDay(2026, 6, 22, 900),
	}
	if got := SeamBreak(stored, []Candle{seamDay(2026, 6, 23, 910)}); got != "" {
		t.Fatalf("過去の実暴落で更新が止まった(銘柄が恒久ロックされる): %q", got)
	}
}

// 継ぎ目の窓 = [最後の保存バー] + [それより新しい取得バー](取得は順不同でよい)。
func TestSeamWindow(t *testing.T) {
	stored := []Candle{seamDay(2026, 6, 19, 1), seamDay(2026, 6, 22, 2)}
	fetched := []Candle{seamDay(2026, 6, 24, 4), seamDay(2026, 6, 22, 9), seamDay(2026, 6, 23, 3)}
	got := SeamWindow(stored, fetched)
	var closes []float64
	for _, c := range got {
		closes = append(closes, c.Close)
	}
	if len(closes) != 3 || closes[0] != 2 || closes[1] != 3 || closes[2] != 4 {
		t.Fatalf("seam window closes = %v, want [2 3 4]", closes)
	}
}
