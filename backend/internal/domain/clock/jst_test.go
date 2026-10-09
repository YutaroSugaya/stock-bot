package clock

import (
	"testing"
	"time"
)

// JST は tzdata が読めない環境でも +09:00 を返す。fail-open で UTC に落ちると
// 日足の暦日境界と 14:50 引け前フラット化が 9 時間ずれる。
func TestJSTIsAlwaysNineHoursAhead(t *testing.T) {
	if JST == nil {
		t.Fatal("JST が nil")
	}
	// JPX が扱う年代に DST は無い(日本の夏時間は 1951 年まで)。
	for _, at := range []time.Time{
		time.Date(2008, 1, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
	} {
		if _, off := at.In(JST).Zone(); off != 9*3600 {
			t.Errorf("%s のオフセットが %d 秒(9時間ではない)", at.Format("2006-01-02"), off)
		}
	}
	// 同じ壁時計を JST で解釈したら UTC より 9 時間早い瞬間になる。
	jst := time.Date(2026, 8, 13, 9, 0, 0, 0, JST)
	utc := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	if d := utc.Sub(jst); d != 9*time.Hour {
		t.Errorf("UTC - JST = %v(9h ではない)", d)
	}
}
