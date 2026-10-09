package query_test

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/query"
)

func extendHours(t *testing.T) session.TradingHours {
	t.Helper()
	tz, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	return session.TradingHours{
		TZ:          tz,
		Sessions:    []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:30"}},
		ForceFlatAt: "14:50",
		Holidays:    map[string]struct{}{"2026-10-12": {}}, // スポーツの日
		// 10/20 までしか休場日を知らない = それより先の候補は祝日を確かめていない。
		CalendarThrough: time.Date(2026, 10, 20, 0, 0, 0, 0, tz),
	}
}

// 期限 10/1 14:50:10 の多日建玉の形。延長候補は「各営業日の 14:50」で、
// add_minutes は**今の期限から**の分(切り上げ)。1 回の上限(30 日)より先は出さない。
func TestListExtendOptions_MultidayOffersEachBusinessDayUpToTheCap(t *testing.T) {
	ctx := context.Background()
	th := extendHours(t)
	tz := th.TZ
	repo := repository.NewInMemoryPositionRepo()
	opened := time.Date(2026, 9, 17, 14, 50, 10, 0, tz)
	until := time.Date(2026, 10, 1, 14, 50, 10, 0, tz)
	id, err := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "p", Symbol: "4901", Side: order.SideBuy, Quantity: 100,
		OpenedAt: opened, HoldingMode: order.HoldingMultiday,
		MaxHoldMinutes: int(until.Sub(opened).Minutes()),
	})
	if err != nil {
		t.Fatal(err)
	}

	v, err := query.NewListExtendOptions(repo, th).Execute(ctx, id)
	if err != nil || v == nil {
		t.Fatalf("got (%+v, %v)", v, err)
	}
	if v.CurrentUntil == nil || !v.CurrentUntil.Equal(until) {
		t.Fatalf("current_until = %v, want %v", v.CurrentUntil, until)
	}
	if v.MaxAddMinutes != 30*24*60 {
		t.Errorf("max_add_minutes = %d, want 30日", v.MaxAddMinutes)
	}
	if len(v.Options) == 0 {
		t.Fatal("候補が 0 本")
	}

	first := v.Options[0]
	if !first.Until.Equal(time.Date(2026, 10, 2, 14, 50, 10, 0, tz)) || first.AddMinutes != 1440 || first.BusinessDays != 1 {
		t.Errorf("first = %+v, want 10/2 14:50:10 / +1440分 / 1営業日", first)
	}
	for i, o := range v.Options {
		if o.AddMinutes < 1 || o.AddMinutes > v.MaxAddMinutes {
			t.Errorf("[%d] add_minutes %d は 1 回の上限の外", i, o.AddMinutes)
		}
		// 延長後の期限 = 今の期限 + add_minutes(domain の MaxHoldUntil と同じ式)。
		if !o.Until.Equal(until.Add(time.Duration(o.AddMinutes) * time.Minute)) {
			t.Errorf("[%d] until %v と add_minutes %d が合わない", i, o.Until, o.AddMinutes)
		}
		if at := o.Until.In(tz); at.Hour() != 14 || at.Minute() != 50 {
			t.Errorf("[%d] %v は引け前ではない", i, o.Until)
		}
		if wd := o.Until.In(tz).Weekday(); wd == time.Saturday || wd == time.Sunday {
			t.Errorf("[%d] %v は週末", i, o.Until)
		}
		if o.Until.In(tz).Format("2006-01-02") == "2026-10-12" {
			t.Errorf("[%d] 休場日 10/12 が候補に入った", i)
		}
		wantBeyond := o.Until.In(tz).Format("2006-01-02") > "2026-10-20"
		if o.BeyondCalendar != wantBeyond {
			t.Errorf("[%d] %v beyond_calendar = %v, want %v", i, o.Until, o.BeyondCalendar, wantBeyond)
		}
	}
	last := v.Options[len(v.Options)-1]
	if !last.Until.Equal(time.Date(2026, 10, 30, 14, 50, 10, 0, tz)) {
		t.Errorf("last = %v, want 10/30(10/31 は土曜・30 日の上限内の最終営業日)", last.Until)
	}
}

// intraday は 12 時間が上限なので、候補は当日の引け前だけになる。
func TestListExtendOptions_IntradayOnlyReachesTheSameDayForceFlat(t *testing.T) {
	ctx := context.Background()
	th := extendHours(t)
	tz := th.TZ
	repo := repository.NewInMemoryPositionRepo()
	opened := time.Date(2026, 9, 25, 9, 30, 0, 0, tz)
	id, _ := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "i", Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		OpenedAt: opened, HoldingMode: order.HoldingIntraday, MaxHoldMinutes: 60,
	})
	v, err := query.NewListExtendOptions(repo, th).Execute(ctx, id)
	if err != nil || v == nil {
		t.Fatalf("got (%+v, %v)", v, err)
	}
	if len(v.Options) != 1 || !v.Options[0].Until.Equal(time.Date(2026, 9, 25, 14, 50, 0, 0, tz)) || v.Options[0].BusinessDays != 0 {
		t.Fatalf("options = %+v, want [当日 14:50]", v.Options)
	}
}

// 期限の無い建玉(max_hold 0 = 無期限)に延長を足すと**期限を新しく作ってしまう**。候補は出さない。
func TestListExtendOptions_NoDeadlineMeansNoOptions(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryPositionRepo()
	id, _ := repo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "x", Symbol: "7203", Side: order.SideBuy, Quantity: 100,
		OpenedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC), HoldingMode: order.HoldingMultiday,
	})
	v, err := query.NewListExtendOptions(repo, extendHours(t)).Execute(ctx, id)
	if err != nil || v == nil {
		t.Fatalf("got (%+v, %v)", v, err)
	}
	if v.CurrentUntil != nil || len(v.Options) != 0 || v.Options == nil {
		t.Fatalf("got %+v, want current_until=nil / options=[]", v)
	}
}

// 未知 id は (nil, nil) — handler が 404 に写す(延長 API と同じ契約)。
func TestListExtendOptions_UnknownIDIsNil(t *testing.T) {
	v, err := query.NewListExtendOptions(repository.NewInMemoryPositionRepo(), extendHours(t)).Execute(context.Background(), 42)
	if err != nil || v != nil {
		t.Fatalf("got (%+v, %v), want (nil, nil)", v, err)
	}
}
