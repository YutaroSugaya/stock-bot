package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/testutil"
)

type flakyRefresher struct {
	failTimes int
	calls     int
}

func (f *flakyRefresher) RefreshToken(context.Context) error {
	f.calls++
	if f.calls <= f.failTimes {
		return errors.New("broker down")
	}
	return nil
}

func TestInTachibanaMaintenanceWindow(t *testing.T) {
	cases := []struct {
		h    int
		want bool
	}{{2, false}, {3, true}, {4, true}, {5, true}, {6, false}, {9, false}}
	for _, c := range cases {
		got := inTachibanaMaintenanceWindow(time.Date(2026, 6, 17, c.h, 0, 0, 0, clock.JST))
		if got != c.want {
			t.Fatalf("tachibana window @%02d:00 = %v, want %v", c.h, got, c.want)
		}
	}
}

func TestMaintenanceWindowFor(t *testing.T) {
	at3 := time.Date(2026, 6, 17, 3, 0, 0, 0, clock.JST) // 立花 API 閉局 03:30〜
	if !maintenanceWindowFor(config.BrokerTachibana)(at3) {
		t.Fatal("tachibana window must include 03:00")
	}
}

func TestRefreshWithRetry_SucceedsAfterTransient(t *testing.T) {
	// 一時的な失敗で emergency に上げない(閉局・瞬断は毎日起きる)。
	r := &flakyRefresher{failTimes: 2}
	ok := refreshWithRetry(context.Background(), r, 4, time.Millisecond, testutil.SilentLogger())
	if !ok {
		t.Fatalf("should recover after transient failures; calls=%d", r.calls)
	}
}

func TestRefreshWithRetry_NoEscalateOnCancel(t *testing.T) {
	// shutdown 中の失敗で trip させない。
	r := &flakyRefresher{failTimes: 100}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok := refreshWithRetry(ctx, r, 4, time.Millisecond, testutil.SilentLogger()); !ok {
		t.Fatal("cancelled context must not escalate to a trip")
	}
}

func TestRefreshWithRetry_FailsAfterAllAttempts(t *testing.T) {
	r := &flakyRefresher{failTimes: 100}
	if ok := refreshWithRetry(context.Background(), r, 4, time.Millisecond, testutil.SilentLogger()); ok {
		t.Fatal("persistent failure should return false (escalate)")
	}
}

// reconcile は paper でも回す。live 限定だった頃は、再起動を跨いだ建玉が CLOSING で
// 永久に座礁していた(reconcile 以外どの経路も CLOSING を再訪しない)。
func TestDefaultLoopConfigRunsReconcileInEveryMode(t *testing.T) {
	for _, mode := range []config.Mode{config.ModePaper, config.ModeLive, config.ModeDisabled} {
		if lc := defaultLoopConfig(mode, config.BrokerPaper, false); !lc.reconcile {
			t.Errorf("mode=%s: reconcile ループが回らない(CLOSING 座礁が復旧しない)", mode)
		}
	}
}

// token refresh は 立花(live)だけ — paper には更新するセッションが無い。
func TestDefaultLoopConfigKeepsLiveOnlyFlagForLive(t *testing.T) {
	if defaultLoopConfig(config.ModePaper, config.BrokerPaper, false).live {
		t.Error("paper で live フラグが立っている")
	}
	if !defaultLoopConfig(config.ModeLive, config.BrokerTachibana, false).live {
		t.Error("live で live フラグが立っていない")
	}
}

// 一括取得**していない**実フィードは間引く。1秒 × 監視銘柄数 が立花から
// 高負荷と指摘された原因だったため、ここを戻さないこと。
func TestDefaultLoopConfigThrottlesUnbatchedRealFeeds(t *testing.T) {
	for _, k := range []config.BrokerKind{config.BrokerTachibana, config.BrokerPaperLiveFeed} {
		got := defaultLoopConfig(config.ModePaper, k, false /*batched*/).priceInterval
		if got < realFeedPriceInterval {
			t.Errorf("broker=%s: priceInterval=%v — 1銘柄1リクエストで %v 未満は不可", k, got, realFeedPriceInterval)
		}
	}
}

// 一括取得できても 1 秒には戻さない(立会中ずっと 1 req/s = 19,800回/日)。
// 「どこまで詰めてよいか」は間隔ではなく**1日の回数**で縛る —
// TestInSessionQuoteRequestsStayWithinBudget が SSOT。
func TestDefaultLoopConfigUsesBatchedIntervalWhenBatched(t *testing.T) {
	got := defaultLoopConfig(config.ModePaper, config.BrokerPaperLiveFeed, true).priceInterval
	if got != batchedPriceInterval {
		t.Errorf("batched priceInterval=%v, want %v", got, batchedPriceInterval)
	}
}

// 立会中の時価取得が1日の予算内に収まること。**間隔ではなく回数**を検査する:
// 旧ガードは priceInterval >= 5秒 という代理指標だったので、別レーンを足して実際の
// 呼び出しが 3,960 → 9,570回/日 に増えても素通りしていた(実測)。
func TestInSessionQuoteRequestsStayWithinBudget(t *testing.T) {
	// 検査対象は**出荷時の既定値**。env が残った環境で落ちる env 依存のテストにしない
	// (実行時に env で詰めること自体は禁じていない)。
	t.Setenv("STOCKBOT_PRICE_INTERVAL_SEC", "")
	lc := defaultLoopConfig(config.ModePaper, config.BrokerPaperLiveFeed, true)
	got := inSessionQuoteRequests(lc, 1)
	if got > dailyQuoteRequestBudget {
		t.Errorf("立会中の時価取得 %d回/日 > 予算 %d回/日 (price=%v)",
			got, dailyQuoteRequestBudget, lc.priceInterval)
	}
}

// 🛑 監視銘柄が 1 リクエストの上限(120)を超えてチャンクが増えても、1日の総リクエスト数は
// 変わらないこと。旧ガードはチャンク数を数えていなかったので、**実際の通信量が倍・3倍に
// なっても緑のまま通っていた**。建玉は決済まで積み上がるので
// 「いつか踏む」ではなく「いつ踏むか」の問題だった。
func TestInSessionQuoteRequestsStayFlatAsChunksGrow(t *testing.T) {
	t.Setenv("STOCKBOT_PRICE_INTERVAL_SEC", "")
	lc := defaultLoopConfig(config.ModePaper, config.BrokerPaperLiveFeed, true)
	base := inSessionQuoteRequests(lc, 1)
	// 120銘柄=1, 240銘柄=2, 1,200銘柄=10 チャンク。どれも同じ回数に収まること。
	for _, chunks := range []int{1, 2, 3, 10} {
		got := inSessionQuoteRequests(lc, chunks)
		if got != base {
			t.Errorf("chunks=%d: %d回/日, want %d(監視銘柄数で通信量が変わってはいけない)", chunks, got, base)
		}
		if got > dailyQuoteRequestBudget {
			t.Errorf("chunks=%d: %d回/日 > 予算 %d回/日", chunks, got, dailyQuoteRequestBudget)
		}
	}
}

// 一括取得は 1リクエストに120銘柄まで積めるので、回数は銘柄数ではなく頻度で決まる。
func TestInSessionQuoteRequestsCountsTheLane(t *testing.T) {
	single := inSessionQuoteRequests(loopConfig{priceInterval: 3 * time.Second}, 1)
	if want := tradingSessionSeconds / 3; single != want {
		t.Errorf("単一レーン3秒: %d回/日, want %d", single, want)
	}
	// 予算ガードなので秒未満でも 0 除算で落ちないこと。
	if got := inSessionQuoteRequests(loopConfig{priceInterval: 500 * time.Millisecond}, 1); got != 2*tradingSessionSeconds {
		t.Errorf("500ms: %d回/日, want %d", got, 2*tradingSessionSeconds)
	}
	if got := inSessionQuoteRequests(loopConfig{}, 1); got != 0 {
		t.Errorf("レーン無し: %d回/日, want 0", got)
	}
}

// 🛑 予算は**時価レーンだけでは足りない**。live 配線は口座照会(建玉 reconcile /
// 維持率)も叩くので、時価しか数えない予算ガードは実数が予算の5倍でも緑のまま通る
// (hybrid Step 0-(d))。合計で `dailyQuoteRequestBudget` を守らせる。
func TestInSessionRequestsStayWithinBudget(t *testing.T) {
	t.Setenv("STOCKBOT_PRICE_INTERVAL_SEC", "")
	// live track の最悪ケース: 一般信用(照会が現物+信用の2本になる)で、
	// 場中ずっと建玉を持ち、口座上限まで決済が回った日。
	lc := defaultLoopConfig(config.ModeLive, config.BrokerTachibana, true)
	got := inSessionRequests(lc, 1, true /*marginEnabled*/, liveClosesPerDayAllowance, liveEntryAttemptsPerDayAllowance)
	if got > dailyQuoteRequestBudget {
		t.Errorf("場中の合計 %d回/日 > 予算 %d回/日 (price=%v reconcile=%v margin=%v)",
			got, dailyQuoteRequestBudget, lc.priceInterval, lc.reconcileHeldInterval, lc.marginHeldInterval)
	}
	// 時価だけを数えた値と一致してはいけない = 口座レーンが本当に足されていること。
	if quotes := inSessionQuoteRequests(lc, 1); got <= quotes {
		t.Errorf("合計 %d が時価だけの %d を超えていない(口座照会が数えられていない)", got, quotes)
	}
}

// 口座照会は**建玉があるときだけ**回す(無保有なら朝の同期1周だけ)。規則は
// 保有中は1時間に1回・決済したらその都度・無保有は朝1回。値段の変動は
// ローカル計算で追うので、同じ質問を毎ティック broker に投げる理由が無い。
func TestInSessionAccountRequestsCountsEveryLane(t *testing.T) {
	lc := loopConfig{reconcileHeldInterval: time.Hour, marginHeldInterval: time.Hour}
	hours := tradingSessionSeconds / 3600
	// 現物のみ(信用無効)= どちらの照会も 1リクエスト。起動1周 + 建玉5 + 維持率5。
	if got, want := inSessionAccountRequests(lc, false, 0, 0), 1+2*hours; got != want {
		t.Errorf("現物のみ: %d回/日, want %d", got, want)
	}
	// 🛑 制度信用では**2つのレーンで1照会あたりの本数が違う**。
	//   建玉照会 = 現物 + 信用建玉         → 2本
	//   余力照会 = 買付余力 + 建余力 + 保証金率 → 3本
	// 同じ関数で両方を 2本 と数えていたため、余力レーンを 1/3 過小に見積もっていた。
	if got, want := inSessionAccountRequests(lc, true, 0, 0), 2+2*hours+3*hours; got != want {
		t.Errorf("制度信用: %d回/日, want %d", got, want)
	}
	// 決済都度の再同期(建玉照会 + 維持率)が本数ぶん足される。
	base := inSessionAccountRequests(lc, true, 0, 0)
	if got, want := inSessionAccountRequests(lc, true, 3, 0), base+3*(2+3); got != want {
		t.Errorf("決済3本: %d回/日, want %d", got, want)
	}
	// エントリー試行(構造ゲートを通って担保チェックまで来たもの)は 1試行 = 余力照会1回。
	// 以前はこの項が**存在しなかった**ため、実測 3,011回/日 を予算 50回/日 と
	// 見積もっていた。試行数を数えないモデルは、枠が満杯の live で必ず外れる。
	if got, want := inSessionAccountRequests(lc, true, 0, 4), base+4*3; got != want {
		t.Errorf("エントリー試行4回: %d回/日, want %d", got, want)
	}
	// レーンを止めれば起動時の同期1周だけが残る(この1周は落とさない —
	// 再起動直後のナンピン禁止の盲点を塞ぐ既存の契約)。
	if got := inSessionAccountRequests(loopConfig{}, false, 0, 0); got != 1 {
		t.Errorf("レーン無し: %d回/日, want 1(寄り前の同期1周は残る)", got)
	}
}

// 🛑 live の建玉照会は「場中 × 建玉あり」のときだけ broker を叩く。深夜に口座を
// 引く理由が無く(現行は 24 時間 30 秒ごと)、無保有なら答えは常に空。
// **paper は据え置き**: 紙の帳簿はプロセス内の変数なので API コストがゼロで、
// CLOSING 座礁の回収経路をわざわざ遅くする理由が無い。
func TestReconcileSchedule(t *testing.T) {
	lc := defaultLoopConfig(config.ModeLive, config.BrokerTachibana, true)
	cases := []struct {
		name             string
		live, sess, held bool
		wantPoll         bool
		wantWait         time.Duration
	}{
		{"paper は常に従来どおり", false, false, false, true, lc.reconcileInterval},
		{"live 場中×建玉あり", true, true, true, true, lc.reconcileHeldInterval},
		{"live 場中×無保有", true, true, false, false, idleReconcileWait},
		{"live 場外×建玉あり", true, false, true, false, idleReconcileWait},
		{"live 場外×無保有", true, false, false, false, idleReconcileWait},
	}
	for _, c := range cases {
		wait, poll := reconcileSchedule(lc, c.live, c.sess, c.held)
		if poll != c.wantPoll {
			t.Errorf("%s: poll=%v, want %v", c.name, poll, c.wantPoll)
		}
		if wait != c.wantWait {
			t.Errorf("%s: wait=%v, want %v", c.name, wait, c.wantWait)
		}
	}
}

// 🛑 場外・無保有でも**目は覚ます**(建玉が入った/寄り付いたのを拾うため)。
// 目覚ましそのものは broker を叩かないので予算には乗らない。
func TestIdleReconcileWaitCostsNoRequests(t *testing.T) {
	if idleReconcileWait <= 0 {
		t.Fatal("idleReconcileWait が 0 だと runVarTicker が全力ループになる")
	}
	if _, poll := reconcileSchedule(loopConfig{}, true, false, false); poll {
		t.Error("場外・無保有で poll=true(深夜に口座照会が飛ぶ)")
	}
}

// 既定は「保有中は1時間」。毎ティック / 30秒に戻す変更をここで止める。
func TestDefaultLoopConfigAccountPollingIsHourly(t *testing.T) {
	lc := defaultLoopConfig(config.ModeLive, config.BrokerTachibana, true)
	if lc.reconcileHeldInterval != time.Hour {
		t.Errorf("reconcileHeldInterval=%v, want 1h", lc.reconcileHeldInterval)
	}
	if lc.marginHeldInterval != time.Hour {
		t.Errorf("marginHeldInterval=%v, want 1h", lc.marginHeldInterval)
	}
}

// 合成フィード(paper 単体)は外部 API を叩かないので刻んでよい。
func TestDefaultLoopConfigKeepsPaperFast(t *testing.T) {
	if got := defaultLoopConfig(config.ModePaper, config.BrokerPaper, false).priceInterval; got != time.Second {
		t.Errorf("paper priceInterval=%v, want 1s(外部 API を叩かない)", got)
	}
}

// 人間が調整できる逃げ道。値が壊れていたら既定に落とす。
func TestPriceIntervalEnvOverride(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", batchedPriceInterval},
		{"10", 10 * time.Second}, // 既定と違う値でないと上書きを検査したことにならない
		{"2", 2 * time.Second},
		{"0", batchedPriceInterval},
		{"-3", batchedPriceInterval},
		{"abc", batchedPriceInterval},
	}
	for _, tc := range cases {
		t.Setenv("STOCKBOT_PRICE_INTERVAL_SEC", tc.env)
		if got := defaultLoopConfig(config.ModePaper, config.BrokerPaperLiveFeed, true).priceInterval; got != tc.want {
			t.Errorf("env=%q: priceInterval=%v, want %v", tc.env, got, tc.want)
		}
	}
}

// 🚨 起動時 reconcile が落ちて entry を保留している銘柄は、保有中の間隔(1 時間)で
// 待たずに idleReconcileWait で取り直す。1 時間待つと、場中に
// 再起動した日はその銘柄が最大 1 時間建たない。保留中でなければ間隔は変えない。
func TestReconcileRetryWhileEntriesHeld(t *testing.T) {
	if got := retryWhileHeld(time.Hour, true); got != idleReconcileWait {
		t.Errorf("保留中の待ち=%v, want %v", got, idleReconcileWait)
	}
	if got := retryWhileHeld(time.Hour, false); got != time.Hour {
		t.Errorf("保留していないのに間隔が変わった: %v", got)
	}
	if got := retryWhileHeld(30*time.Second, true); got != 30*time.Second {
		t.Errorf("元の間隔が短いときに伸ばした: %v", got)
	}
}

// 🛑 時価は**全銘柄 1 本のレーン**(D-7: Tier A の別レーンは撤去)。別レーンは回数を足し算で
// 増やすだけで、守れる解像度は数銘柄ぶんだった(立花から回数の上限を指摘されたときの教訓)。
// 旧 env(STOCKBOT_HOT_INTERVAL_SEC)が残った機体でも 2 本目のレーンは生まれない。
func TestQuoteLoopIsASingleLaneEvenWithTheOldHotEnv(t *testing.T) {
	t.Setenv("STOCKBOT_PRICE_INTERVAL_SEC", "")
	t.Setenv("STOCKBOT_HOT_INTERVAL_SEC", "1")
	lc := defaultLoopConfig(config.ModePaper, config.BrokerPaperLiveFeed, true)
	want := int(tradingSessionSeconds * time.Second / batchedPriceInterval)
	if got := inSessionQuoteRequests(lc, 1); got != want {
		t.Fatalf("時価 %d回/日, want 単一レーン %d回/日", got, want)
	}
}
