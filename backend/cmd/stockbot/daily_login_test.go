package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/testutil"
)

// 🚨 立花はログインを 1 日 1 回に留めるよう求めている(仮想URL は 1 日に 1 度取得すれば
// 該当営業日は継続利用できる)。旧ループは毎時 logout+login していた。
// 日次の張り直しは**集計窓(5:30 起点)にまだ 1 度もログインしていないとき**だけにする。

func octJST(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, clock.JST) }

func TestNeedsDailyLogin(t *testing.T) {
	cases := []struct {
		name      string
		now, last time.Time
		want      bool
	}{
		{"同じ窓の中では張り直さない(場中)", octJST(1, 12, 0), octJST(1, 8, 46), false},
		{"同じ窓の中では張り直さない(引け後)", octJST(1, 23, 0), octJST(1, 8, 46), false},
		{"窓をまたいだら張り直す(閉局明け)", octJST(2, 6, 10), octJST(1, 12, 15), true},
		{"閉局中は張り直さない", octJST(2, 4, 0), octJST(1, 12, 15), false},
		{"窓は 5:30 からだが 6 時までは閉局扱い", octJST(2, 5, 45), octJST(1, 12, 15), false},
		{"窓の中で既に張り直していれば撃たない(p_errno=2 で張った)", octJST(2, 6, 10), octJST(2, 5, 40), false},
		{"未ログイン(ゼロ値)は張る", octJST(2, 9, 0), time.Time{}, true},
	}
	for _, c := range cases {
		if got := needsDailyLogin(c.now, c.last, inTachibanaMaintenanceWindow); got != c.want {
			t.Errorf("%s: needsDailyLogin(now=%s, last=%s) = %v, want %v",
				c.name, c.now.Format("01-02 15:04"), c.last.Format("01-02 15:04"), got, c.want)
		}
	}
}

// fakeSession は RefreshToken が成功したときだけ最終ログイン時刻を進める(立花と同じ)。
type fakeSession struct {
	now      func() time.Time
	last     time.Time
	calls    int
	failures int // 先頭から何回失敗させるか
}

func (f *fakeSession) RefreshToken(context.Context) error {
	f.calls++
	if f.calls <= f.failures {
		return errBrokerDownForTest
	}
	f.last = f.now()
	return nil
}

var errBrokerDownForTest = errors.New("broker down")

// main は立花のセッションから最終ログイン時刻を引く。外れると日次の張り直しが回らない。
var _ interface{ LastLoginAt() time.Time } = (*broker.Tachibana)(nil)

// 場中に一日中ループを回しても、起動時の 1 回のほかにログインしない。
func TestDailyLoginTick_NoLoginsWithinTheWindow(t *testing.T) {
	now := octJST(1, 8, 46)
	s := &fakeSession{now: func() time.Time { return now }}
	s.last = now // 起動時のログイン
	tripped := ""
	for now.Before(octJST(2, 3, 0)) {
		dailyLoginTick(context.Background(), s, now, func() time.Time { return s.last },
			inTachibanaMaintenanceWindow, func(r string) { tripped = r }, time.Millisecond, testutil.SilentLogger())
		now = now.Add(dailyLoginCheckInterval)
	}
	if s.calls != 0 {
		t.Fatalf("同じ窓の中で %d 回ログインした(立花の求めは 1 日 1 度)", s.calls)
	}
	if tripped != "" {
		t.Fatalf("trip した: %s", tripped)
	}
}

// 閉局をまたいで動き続けると、窓が替わった後に**ちょうど 1 回**張り直す。
func TestDailyLoginTick_ExactlyOneLoginAfterWindowRollover(t *testing.T) {
	now := octJST(1, 12, 15)
	s := &fakeSession{now: func() time.Time { return now }}
	s.last = now
	for now.Before(octJST(2, 15, 30)) {
		dailyLoginTick(context.Background(), s, now, func() time.Time { return s.last },
			inTachibanaMaintenanceWindow, func(string) { t.Fatal("trip した") }, time.Millisecond, testutil.SilentLogger())
		now = now.Add(dailyLoginCheckInterval)
	}
	if s.calls != 1 {
		t.Fatalf("窓をまたいだ張り直し = %d 回, want 1", s.calls)
	}
	if h := s.last.In(clock.JST).Hour(); h != 6 {
		t.Fatalf("張り直したのが %s(閉局明けの 6 時台であるべき)", s.last.In(clock.JST).Format("15:04"))
	}
}

// 閉局明けの張り直しが再試行しても通らなければ緊急停止(従来の token_refresh_failed と同じ)。
func TestDailyLoginTick_TripsWhenTheDailyLoginKeepsFailing(t *testing.T) {
	now := octJST(2, 6, 10)
	s := &fakeSession{now: func() time.Time { return now }, failures: 100}
	s.last = octJST(1, 12, 15)
	tripped := ""
	dailyLoginTick(context.Background(), s, now, func() time.Time { return s.last },
		inTachibanaMaintenanceWindow, func(r string) { tripped = r }, time.Millisecond, testutil.SilentLogger())
	if tripped != "token_refresh_failed" {
		t.Fatalf("trip = %q, want token_refresh_failed", tripped)
	}
	if s.calls != 4 {
		t.Fatalf("再試行 = %d 回, want 4", s.calls)
	}
}
