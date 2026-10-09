package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 立花には 1 日の API 利用回数の上限があり、上限の内側に収まっている**証拠**が
// 必要なのに、実際の wire 回数を数える場所がどこにも無かった。
//
// 推定では足りない理由: プール拡張で fetch-daily が
// 222 → 1,551 リクエスト/日 に増えていたのに、誰も気づかないまま数日運用していた。
// 見積りは黙って古くなる。**数えたものだけが分かる。**
//
// 数えるのは doGET(唯一の HTTP 送信点)なので、login も p_errno=2 の再送も
// チャンク分割も**全部**入る。相手のサーバから見た回数と一致する数え方にする
// (「論理的には1回の一括取得」で数えると、120銘柄超のチャンク分割を見落とす)。
func TestAPIRequestsCountsEveryWireRequest(t *testing.T) {
	var srvHits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		srvHits++
		mu.Unlock()
		w.Write([]byte(`{"p_no":"1","p_errno":"0","sResultCode":"0"}`))
	}))
	defer srv.Close()

	tc := newWireCountTestClient(t, srv.URL)

	if got := tc.APIRequests(); got != 0 {
		t.Fatalf("初期値 %d, want 0", got)
	}

	const n = 3
	for i := 0; i < n; i++ {
		var out commonResp
		if _, err := tc.requestOnce(context.Background(), urlPrice, tachiCLMMarketPrice,
			map[string]string{"sTargetIssueCode": "7203"}, &out); err != nil {
			t.Fatalf("requestOnce: %v", err)
		}
	}

	mu.Lock()
	hits := srvHits
	mu.Unlock()
	if hits != n {
		t.Fatalf("サーバ実測 %d 回, want %d — テストの前提が崩れている", hits, n)
	}
	if got := tc.APIRequests(); got != int64(n) {
		t.Errorf("APIRequests()=%d, サーバ実測=%d — 相手から見た回数と一致していない", got, hits)
	}
}

// 一括取得は 120銘柄で分割される(立花の上限・本番実測)。
// 「1回の GetTickers」ではなく**送った本数**を数える。ここを論理回数で数えると、
// 監視銘柄が 120 を超えた瞬間に実際の負荷が倍になっても数字が動かない。
func TestAPIRequestsCountsEachChunk(t *testing.T) {
	var mu sync.Mutex
	var gotCodes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotCodes = append(gotCodes, r.URL.RawQuery)
		mu.Unlock()
		// 中身は問わない(数える対象は送信回数)。空の一覧を返す。
		w.Write([]byte(`{"p_no":"1","p_errno":"0","aCLMMfdsMarketPrice":[]}`))
	}))
	defer srv.Close()

	tc := newWireCountTestClient(t, srv.URL)
	tc.batchSize = 2 // 上限を小さくして分割を再現する

	syms := []string{"1001", "1002", "1003", "1004", "1005"} // 2+2+1 = 3 チャンク
	if _, err := tc.GetTickers(context.Background(), syms); err != nil {
		t.Fatalf("GetTickers: %v", err)
	}

	mu.Lock()
	sent := len(gotCodes)
	mu.Unlock()
	if sent != 3 {
		t.Fatalf("送信 %d 本, want 3 — 分割の前提が崩れている", sent)
	}
	if got := tc.APIRequests(); got != 3 {
		t.Errorf("APIRequests()=%d, want 3 — GetTickers 1回を1回と数えている(分割を見落とす)", got)
	}
}

// 送信できなかった試行も数える。相手のサーバには届いているのに手元で失敗した
// 場合(タイムアウト・切断)を差し引くと、**broker 側から見た回数より少なく**数える
// ことになる。過少に数えた値は上限の内側にいる証拠として使えない。
func TestAPIRequestsCountsFailedSends(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("hijack 不可")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		conn.Close() // 応答せず切断
	}))
	defer srv.Close()

	tc := newWireCountTestClient(t, srv.URL)
	var out commonResp
	_, err := tc.requestOnce(context.Background(), urlPrice, tachiCLMMarketPrice,
		map[string]string{"sTargetIssueCode": "7203"}, &out)
	if err == nil {
		t.Fatal("エラーを期待したが nil — テストの前提が崩れている")
	}
	if got := tc.APIRequests(); got != 1 {
		t.Errorf("APIRequests()=%d, want 1 — 失敗した送信を数えていない(過少申告になる)", got)
	}
}

// 価格ループは並行に走る。数え落ち/二重計上が出ると申告値がずれるので atomic で
// 数えていることを固定する(-race と併せて)。
func TestAPIRequestsIsConcurrencySafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"p_no":"1","p_errno":"0","sResultCode":"0"}`))
	}))
	defer srv.Close()

	tc := newWireCountTestClient(t, srv.URL)
	const goroutines, each = 8, 10
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				var out commonResp
				_, _ = tc.requestOnce(context.Background(), urlPrice, tachiCLMMarketPrice,
					map[string]string{"sTargetIssueCode": "7203"}, &out)
			}
		}()
	}
	wg.Wait()
	if got, want := tc.APIRequests(), int64(goroutines*each); got != want {
		t.Errorf("APIRequests()=%d, want %d", got, want)
	}
}

// newWireCountTestClient builds a Tachibana whose session points at srvURL and
// whose rate limiter is out of the way (ここで測りたいのは数え方であって速度制限
// ではない)。
func newWireCountTestClient(t *testing.T, srvURL string) *Tachibana {
	t.Helper()
	if strings.TrimSpace(srvURL) == "" {
		t.Fatal("srvURL が空")
	}
	tc := NewTachibana("demo", "authid", nil, "pw", false, false, nil)
	tc.SetRateLimit(0, 0) // 測るのは数え方であって速度制限ではない
	// ログイン済みセッションを直接注入する(仮想 URL のパス自体がトークン)。
	tc.session = &tachiSession{requestURL: srvURL, masterURL: srvURL, priceURL: srvURL}
	return tc
}

// fakeUsage は「立花と同じ集計単位で数える」外部カウンタの fake。
type fakeUsage struct {
	mu    sync.Mutex
	byCLM map[string]int
}

func (f *fakeUsage) Record(clmid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byCLM == nil {
		f.byCLM = map[string]int{}
	}
	f.byCLM[clmid]++
}

func (f *fakeUsage) count(clmid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byCLM[clmid]
}

// 🛑 立花の内訳は **CLMID 単位**で届く(MarketPrice / History / Logout の別)。
// 総数しか持っていないと「どの処理が原因か」に二度と答えられないので、
// 永続カウンタへは CLMID を添えて渡すこと。数え口は APIRequests と同じ doGET
// (= 相手から見た回数)であること。
func TestUsageRecorderSeesEveryWireRequestWithItsCLMID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"p_no":"1","p_errno":"0","sResultCode":"0"}`))
	}))
	defer srv.Close()

	tc := newWireCountTestClient(t, srv.URL)
	usage := &fakeUsage{}
	tc.SetUsageRecorder(usage)

	for i := 0; i < 3; i++ {
		var out commonResp
		if _, err := tc.requestOnce(context.Background(), urlPrice, tachiCLMMarketPrice,
			map[string]string{"sTargetIssueCode": "7203"}, &out); err != nil {
			t.Fatalf("requestOnce: %v", err)
		}
	}
	var out commonResp
	if _, err := tc.requestOnce(context.Background(), urlRequest, tachiCLMLogout, map[string]string{}, &out); err != nil {
		t.Fatalf("logout: %v", err)
	}

	if got := usage.count(tachiCLMMarketPrice); got != 3 {
		t.Errorf("MarketPrice=%d, want 3", got)
	}
	if got := usage.count(tachiCLMLogout); got != 1 {
		t.Errorf("Logout=%d, want 1", got)
	}
	// プロセス内カウンタと同じ数え口であること(片方だけ増える = 定義がずれている)。
	if got := tc.APIRequests(); got != 4 {
		t.Errorf("APIRequests()=%d, want 4 — 2つのカウンタの数え口がずれている", got)
	}
}

// 記録先を挿していない構成(paper / テスト)でも送信が落ちないこと。
func TestWireRequestsWorkWithoutUsageRecorder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"p_no":"1","p_errno":"0","sResultCode":"0"}`))
	}))
	defer srv.Close()

	tc := newWireCountTestClient(t, srv.URL) // recorder 未設定
	var out commonResp
	if _, err := tc.requestOnce(context.Background(), urlPrice, tachiCLMMarketPrice,
		map[string]string{"sTargetIssueCode": "7203"}, &out); err != nil {
		t.Fatalf("requestOnce: %v", err)
	}
	if got := tc.APIRequests(); got != 1 {
		t.Errorf("APIRequests()=%d, want 1", got)
	}
}
