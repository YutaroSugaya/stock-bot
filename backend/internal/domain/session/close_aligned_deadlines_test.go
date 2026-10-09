package session

import (
	"testing"
	"time"
)

// 延長先の候補は「各営業日の引け前(ForceFlatAt)」だけ。画面のセレクトはこの列を出す。
// 休場日と週末は飛ばし、after は含まず through は含む。
func TestCloseAlignedDeadlinesListsEachBusinessDayForceFlat(t *testing.T) {
	th := silverWeekHours(t)
	tz := th.TZ
	// 金曜 9/18 14:50:10 が今の期限(実際の期限と同じく秒が付く)。月曜〜水曜はシルバーウィーク。
	after := time.Date(2026, 9, 18, 14, 50, 10, 0, tz)
	through := time.Date(2026, 9, 28, 14, 50, 10, 0, tz)

	got := th.CloseAlignedDeadlines(after, through)
	want := []time.Time{
		time.Date(2026, 9, 24, 14, 50, 0, 0, tz),
		time.Date(2026, 9, 25, 14, 50, 0, 0, tz),
		time.Date(2026, 9, 28, 14, 50, 0, 0, tz),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// 今の期限が朝(旧来の同時刻ルールの建玉)なら、同じ日の 14:50 も候補に入る。
func TestCloseAlignedDeadlinesIncludesSameDayWhenStillAhead(t *testing.T) {
	th := silverWeekHours(t)
	tz := th.TZ
	after := time.Date(2026, 9, 24, 9, 0, 0, 0, tz)
	got := th.CloseAlignedDeadlines(after, time.Date(2026, 9, 24, 23, 0, 0, 0, tz))
	if len(got) != 1 || !got[0].Equal(time.Date(2026, 9, 24, 14, 50, 0, 0, tz)) {
		t.Fatalf("got %v, want [9/24 14:50]", got)
	}
}

// ForceFlatAt / Sessions が無い呼び手には候補を出さない(時刻を捏造しない)。
func TestCloseAlignedDeadlinesEmptyWithoutForceFlat(t *testing.T) {
	th := businessDaysHours(t) // Sessions / ForceFlatAt 無し
	after := time.Date(2026, 8, 7, 9, 0, 0, 0, th.TZ)
	if got := th.CloseAlignedDeadlines(after, after.AddDate(0, 0, 10)); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}
