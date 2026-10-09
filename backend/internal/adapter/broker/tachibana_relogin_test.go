package broker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
)

// 🚨 立花はログインを 1 日 1 回に留めるよう求めている(仮想URL は 1 日に 1 度取得すれば
// 該当営業日は継続利用できる)。
//
// p_errno=2 を見たリクエストは張り直すが、**張り直しても死んだまま**のとき
// (閉局中・同一 ID の別プロセスとの蹴り合い)に次のリクエストがまた張り直すと、
// 15 分周期の照会が銘柄数ぶんログインを撃つ。張り直しの間隔を空け、失敗が続くほど延ばす。

type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeNow) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeNow) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

func deadSessionRespond(clmid, base string) map[string]any {
	if clmid == tachiCLMKanougaku {
		return map[string]any{"p_errno": "2", "sCLMID": clmid}
	}
	return defaultRespond(clmid, base)
}

func TestTachibana_ReloginOnDeadSessionBacksOff(t *testing.T) {
	tb, rec, ts := newMockTachibana(t, false, deadSessionRespond)
	defer ts.Close()
	fn := &fakeNow{t: time.Date(2026, 10, 2, 9, 0, 0, 0, clock.JST)}
	tb.clock = fn.now
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	logins := func() int { return rec.loginCount() }
	base := logins()

	// 1 本目: 張り直す(従来どおり)。張り直しても死んでいるので error。
	if _, err := tb.GetAccountMargin(ctx); err == nil {
		t.Fatal("死んだセッションで照会が通った")
	}
	if got := logins() - base; got != 1 {
		t.Fatalf("1 本目の張り直し = %d 回, want 1", got)
	}

	// 直後の照会(別銘柄の reconcile 等)は**張り直さない**。ログインを撃たずに error。
	for i := 0; i < 5; i++ {
		_, err := tb.GetAccountMargin(ctx)
		if !errors.Is(err, errTachiReloginThrottled) {
			t.Fatalf("間隔内の照会 %d: err = %v, want errTachiReloginThrottled", i, err)
		}
	}
	if got := logins() - base; got != 1 {
		t.Fatalf("間隔内にログインを撃った: 計 %d 回, want 1", got)
	}

	// 1 分空けば 1 回だけ張り直せる。
	fn.advance(reloginMinInterval)
	_, _ = tb.GetAccountMargin(ctx)
	if got := logins() - base; got != 2 {
		t.Fatalf("1 分後の張り直し: 計 %d 回, want 2", got)
	}

	// 失敗が続いたので次は 2 分空ける(1 分では撃たない)。
	fn.advance(reloginMinInterval)
	_, _ = tb.GetAccountMargin(ctx)
	if got := logins() - base; got != 2 {
		t.Fatalf("続けて失敗したのに 1 分で張り直した: 計 %d 回, want 2", got)
	}
	fn.advance(reloginMinInterval)
	_, _ = tb.GetAccountMargin(ctx)
	if got := logins() - base; got != 3 {
		t.Fatalf("2 分後の張り直し: 計 %d 回, want 3", got)
	}
}

// 間隔の延びには天井がある(閉局明けに何時間も戻らない、を作らない)。
func TestTachibana_ReloginBackoffIsCapped(t *testing.T) {
	tb, rec, ts := newMockTachibana(t, false, deadSessionRespond)
	defer ts.Close()
	fn := &fakeNow{t: time.Date(2026, 10, 2, 3, 30, 0, 0, clock.JST)}
	tb.clock = fn.now
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	base := rec.loginCount()
	// 1 分刻みで 4 時間照会し続ける。
	for i := 0; i < 240; i++ {
		_, _ = tb.GetAccountMargin(ctx)
		fn.advance(time.Minute)
	}
	got := rec.loginCount() - base
	// 1+2+4+8+16 分で 31 分、その後は 30 分ごと → 4 時間で 5 + 7 = 12 回前後。
	if got > 14 {
		t.Fatalf("4 時間で %d 回ログインした(間隔が延びていない)", got)
	}
	if got < 8 {
		t.Fatalf("4 時間で %d 回しか試していない(天井 %v を超えて待っている)", got, reloginMaxInterval)
	}
}

// 静かな期間の後の張り直しは最短の間隔から数え直す(昨日の失敗で今日を待たせない)。
func TestTachibana_ReloginBackoffResetsAfterQuietPeriod(t *testing.T) {
	var mu sync.Mutex
	dead := true
	respond := func(clmid, base string) map[string]any {
		mu.Lock()
		d := dead
		mu.Unlock()
		if d {
			return deadSessionRespond(clmid, base)
		}
		return defaultRespond(clmid, base)
	}
	tb, rec, ts := newMockTachibana(t, false, respond)
	defer ts.Close()
	fn := &fakeNow{t: time.Date(2026, 10, 2, 3, 30, 0, 0, clock.JST)}
	tb.clock = fn.now
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	for i := 0; i < 60; i++ { // 1 時間の失敗で間隔を延ばしておく
		_, _ = tb.GetAccountMargin(ctx)
		fn.advance(time.Minute)
	}
	mu.Lock()
	dead = false
	mu.Unlock()
	fn.advance(6 * time.Hour)

	// 翌営業日の昼にセッションが 1 度だけ死ぬ: 1 回の張り直しで回復する。
	mu.Lock()
	dead = true
	mu.Unlock()
	before := rec.loginCount()
	_, _ = tb.GetAccountMargin(ctx)
	mu.Lock()
	dead = false
	mu.Unlock()
	if got := rec.loginCount() - before; got != 1 {
		t.Fatalf("静かな期間の後の張り直し = %d 回, want 1", got)
	}
	// 1 分後にまた死んでも、待つのは最短の 1 分だけ。
	mu.Lock()
	dead = true
	mu.Unlock()
	fn.advance(reloginMinInterval)
	before = rec.loginCount()
	_, _ = tb.GetAccountMargin(ctx)
	if got := rec.loginCount() - before; got != 1 {
		t.Fatalf("静かな期間の後なのに前日の延びた間隔で待った(張り直し %d 回)", got)
	}
}

// 明示の RefreshToken(起動時・日次の張り直し)は間隔に縛られない —
// 縛ると起動直後の日次判定が「張り直せない」で緊急停止に化ける。
func TestTachibana_ExplicitRefreshIsNotThrottled(t *testing.T) {
	tb, rec, ts := newMockTachibana(t, false, deadSessionRespond)
	defer ts.Close()
	fn := &fakeNow{t: time.Date(2026, 10, 2, 9, 0, 0, 0, clock.JST)}
	tb.clock = fn.now
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	_, _ = tb.GetAccountMargin(ctx) // 張り直し → 間隔に入る
	before := rec.loginCount()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("明示の RefreshToken が間隔で止められた: %v", err)
	}
	if got := rec.loginCount() - before; got != 1 {
		t.Fatalf("明示の RefreshToken のログイン = %d 回, want 1", got)
	}
}

// LastLoginAt は**成功した**ログインの時刻(日次の張り直しが「この窓でもう張ったか」を読む)。
func TestTachibana_LastLoginAtTracksSuccessfulLogin(t *testing.T) {
	tb, _, ts := newMockTachibana(t, false, defaultRespond)
	defer ts.Close()
	fn := &fakeNow{t: time.Date(2026, 10, 2, 8, 30, 0, 0, clock.JST)}
	tb.clock = fn.now
	if !tb.LastLoginAt().IsZero() {
		t.Fatalf("未ログインなのに LastLoginAt = %v", tb.LastLoginAt())
	}
	if err := tb.RefreshToken(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got := tb.LastLoginAt(); !got.Equal(fn.now()) {
		t.Fatalf("LastLoginAt = %v, want %v", got, fn.now())
	}
}

func TestTachibana_LastLoginAtUnchangedOnFailedLogin(t *testing.T) {
	tb, _, ts := newMockTachibana(t, false, defaultRespond)
	fn := &fakeNow{t: time.Date(2026, 10, 2, 8, 30, 0, 0, clock.JST)}
	tb.clock = fn.now
	if err := tb.RefreshToken(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	first := tb.LastLoginAt()
	ts.Close() // 以後の login は届かない
	fn.advance(time.Hour)
	if err := tb.RefreshToken(context.Background()); err == nil {
		t.Fatal("閉じたサーバへの login が成功した")
	}
	if got := tb.LastLoginAt(); !got.Equal(first) {
		t.Fatalf("失敗した login で LastLoginAt が動いた: %v → %v", first, got)
	}
}
