package broker

import (
	"context"
	"sync"
	"testing"
)

// 🚨 毎時の `RefreshToken`(実 logout+login)と 15 分周期の
// 板スイープ / rearm / reconcile は **どれもプロセス起動時刻を起点**にしているので、
// 毎時ちょうど同じ秒に重なる。当日の `p_errno=2` **11 件すべて**が起動から丁度 N 時間の
// 位置にあった(09:25 / 10:28 / 12:54 / 14:54)。
//
// 重なると何が起きるか: p_errno=2 を見たリクエストが**自分でも logout+login しに行く**ため、
// **ループが張り直したばかりの新しいセッションを logout して殺す**。結果は
// 「再ログインしたのにまだ無効」= 照会そのものの失敗で、**裸の建玉に守りを置き直す
// 唯一の自動経路(RearmUnguarded)がその窓で fail-close する**。
//
// 直し方は単一化: セッションに**世代**を持ち、p_errno=2 を見たリクエストは
// **自分が使った世代がまだ現役のときだけ**張り直す。誰かが先に張り直していたら
// 何もせず retry するだけ。

// 既に別の goroutine が張り直していたら、**再ログインしてはいけない**。
// ここで logout+login を撃つのが、新しいセッションを殺していた経路そのもの。
func TestTachibana_RefreshSessionSkipsWhenAnotherGoroutineAlreadyReconnected(t *testing.T) {
	tb, rec, ts := newMockTachibana(t, false, defaultRespond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	stale := tb.sessionGen() // リクエストが「送ったときに使った世代」
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("re-login: %v", err) // ← 毎時のトークン更新ループに相当
	}
	before := rec.loginCount()

	if err := tb.refreshSession(ctx, stale); err != nil {
		t.Fatalf("refreshSession: %v", err)
	}
	if got := rec.loginCount(); got != before {
		t.Fatalf("既に張り直されているのに再ログインした(logins %d → %d)— "+
			"新しいセッションを logout して殺す経路", before, got)
	}
	// 世代が進んでいるだけで、セッションは生きたまま使える。
	if _, err := tb.GetAccountMargin(ctx); err != nil {
		t.Fatalf("張り直し後のセッションで照会できない: %v", err)
	}
}

// 誰も張り直していないなら(= 本当にセッションが死んでいる)、従来どおり 1 回だけ張り直す。
func TestTachibana_RefreshSessionReconnectsWhenNobodyElseDid(t *testing.T) {
	tb, rec, ts := newMockTachibana(t, false, defaultRespond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	before := rec.loginCount()
	if err := tb.refreshSession(ctx, tb.sessionGen()); err != nil {
		t.Fatalf("refreshSession: %v", err)
	}
	if got := rec.loginCount(); got != before+1 {
		t.Fatalf("世代が現役なのに張り直していない(logins %d → %d)", before, got)
	}
}

// 世代はログイン成功のたびに前へ進む(逆流も据え置きもしない)。
// これが単調でないと「誰かが張り直したか」の判定が壊れる。
func TestTachibana_SessionGenerationAdvancesOnEveryLogin(t *testing.T) {
	tb, _, ts := newMockTachibana(t, false, defaultRespond)
	defer ts.Close()
	ctx := context.Background()
	last := tb.sessionGen()
	for i := 0; i < 3; i++ {
		if err := tb.RefreshToken(ctx); err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
		if g := tb.sessionGen(); g <= last {
			t.Fatalf("世代が進んでいない: %d → %d", last, g)
		} else {
			last = g
		}
	}
}

// 🛑 並行して張り直しても壊れない(-race)。単一化していないと、片方の logout が
// もう片方の login 直後のセッションを殺して、その後の照会が落ちる。
func TestTachibana_ConcurrentRefreshLeavesAUsableSession(t *testing.T) {
	tb, _, ts := newMockTachibana(t, false, defaultRespond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tb.RefreshToken(ctx)
		}()
	}
	wg.Wait()
	if _, err := tb.GetAccountMargin(ctx); err != nil {
		t.Fatalf("並行更新の後にセッションが使えない: %v", err)
	}
}

// 一度だけ p_errno=2 を返す死んだセッションは、**再ログイン 1 回**で回復する。
// (「まだ無効」の枝は TestTachibana_SessionInactiveRetryThenError が押さえている)
func TestTachibana_SessionInactiveRecoversWithExactlyOneRelogin(t *testing.T) {
	var mu sync.Mutex
	first := true
	respond := func(clmid, base string) map[string]any {
		if clmid == tachiCLMKanougaku {
			mu.Lock()
			was := first
			first = false
			mu.Unlock()
			if was {
				return map[string]any{"p_errno": "2", "sCLMID": clmid}
			}
		}
		return defaultRespond(clmid, base)
	}
	tb, rec, ts := newMockTachibana(t, false, respond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	before := rec.loginCount()
	if _, err := tb.GetAccountMargin(ctx); err != nil {
		t.Fatalf("再ログインで回復できていない: %v", err)
	}
	if got := rec.loginCount(); got != before+1 {
		t.Fatalf("再ログインが %d 回 (want 1)", got-before)
	}
}
