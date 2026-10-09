package handler_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"

	"stockbot/backend/internal/app/handler"
)

// 副作用のある **全** mutating エンドポイント。
// 🛑 Routes() に POST を足したらここにも足す。ここが不完全だと「ガードを 1 本外した」
// 変更が緑のまま通る(実際 positions/close と flatten-all が漏れていた)。
var mutatingPaths = []string{
	"/api/advisor-trigger",
	"/api/gonogo/run",
	"/api/live/gonogo/run",
	"/api/emergency-stop",
	"/api/emergency-resume",
	"/api/positions/extend",
	"/api/positions/close",
	"/api/flatten-all",
	// live トラック(hybrid)。監査まで**丸ごと漏れていた**。
	"/api/live/positions/close",
	"/api/live/positions/extend",
	// 🚨 **実弾の守りを取り消して置き直す**。守りが一瞬消える窓を作る操作なので、
	// mutating の中でも特に外してはいけない。
	"/api/live/protective/replace",
	// 🛑 守りの**新規設置**。実弾の建玉に返済注文を出す = 資金に触る操作なので
	// 変更系ガードから外さない。
	"/api/live/protective/arm",
	// 🚨 守りを一度板から降ろす = **リスクが一時的に増える**操作。変更系ガード必須。
	"/api/live/protective/reprice",
	"/api/live/emergency-stop",
	"/api/live/emergency-resume",
	// 銘柄ごとの新規停止(人間のボタン)。解除は新規を再開させる = リスクが増える側。
	"/api/live/symbol-blocks",
	"/api/live/symbol-blocks/release",
}

// 🛑 **リストが陳腐化しないようにする**。上の一覧は手で保守する以上、必ず腐る
// (実際 positions/close / flatten-all / live の 3 本 / harvest の 4 本が順に漏れた)。
// `Routes()` のソースから POST の一覧を機械的に取り出し、上の一覧と**完全一致**を要求する。
func TestMutatingPathListCoversEveryPostRoute(t *testing.T) {
	src, err := os.ReadFile("handler.go")
	if err != nil {
		t.Fatalf("read handler.go: %v", err)
	}
	re := regexp.MustCompile(`mux\.HandleFunc\("POST ([^"]+)"`)
	found := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		found[m[1]] = true
	}
	if len(found) == 0 {
		t.Fatal("handler.go から POST ルートを 1 本も抽出できなかった(正規表現が古い)")
	}
	listed := map[string]bool{}
	for _, p := range mutatingPaths {
		listed[p] = true
	}
	for p := range found {
		if !listed[p] {
			t.Errorf("POST %s が mutatingPaths に無い — ガードを外しても全緑になる", p)
		}
	}
	for p := range listed {
		if !found[p] {
			t.Errorf("mutatingPaths の %s が Routes() に無い(ルートが消えたか綴り違い)", p)
		}
	}
}

// triggerHandler は advisor-trigger を配線して 202 を返せる状態の handler。
// 「gate が理由で落ちた」ことを 403 で判別できるようにするため。
func triggerHandler() http.Handler {
	return handler.New(nil, nil, nil, nil, nil, nil).
		WithAdvisorTrigger(func() error { return nil }).
		Routes()
}

// readHandler は WithAdvisorTrigger を**配線しない** handler。
// 🛑 triggerHandler と一本化してはいけない — TestReadsRejectRebinding は read 経路が
// mutating の配線に依存せず判定されることを見ており、trigger を足すと意味が変わる。
func readHandler() http.Handler {
	return handler.New(nil, nil, nil, nil, nil, nil).Routes()
}

// serve は 1 リクエストを組み立てて h に流す。
// 🛑 host が空のときは **Host を触らない**(httptest 既定の example.com のまま)。
// これが「ヘッダも Host も付けない curl / 運用スクリプト」の経路で、素通しが要件。
func serve(h http.Handler, method, path, host string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if host != "" {
		req.Host = host
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func wantCode(t *testing.T, rec *httptest.ResponseRecorder, want int, why string) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (%s, body=%s)", rec.Code, want, why, rec.Body.String())
	}
}

// 127.0.0.1 バインドは「同じマシンの別プロセス」からは守るが、「同じマシンの
// ブラウザで開いた外部ページ」からは守らない。mutating は既定(loopback + token
// 未設定)で無認証なので、悪意ページからの cross-origin simple POST がそのまま
// emergency-resume / extend を叩けてしまう。Content-Type も見ていないので
// preflight も発生しない。ブラウザが必ず送る Sec-Fetch-Site で切る。
//
// curl / スクリプトはこのヘッダを送らないので、運用手順は壊さない(欠落は許可)。
func TestMutationRejectsCrossSiteBrowserRequests(t *testing.T) {
	for _, p := range mutatingPaths {
		t.Run("cross-site は 403: "+p, func(t *testing.T) {
			// Host は **loopback にする**。httptest の既定 Host("example.com")の
			// ままだと Host 検証の方で 403 になり、cross-site 判定を消しても緑の
			// まま = このテストが何も守らなくなる(実際そうなっていた)。
			rec := serve(triggerHandler(), http.MethodPost, p, "127.0.0.1:8090",
				map[string]string{"Sec-Fetch-Site": "cross-site"})
			wantCode(t, rec, http.StatusForbidden, "ブラウザ経由の CSRF が通っている")
		})
	}

	// same-site(同一サイトの別サブドメイン)も拒否する。allowlist を安易に
	// 広げる変更をこのテストが止める。
	t.Run("same-site も 403", func(t *testing.T) {
		rec := serve(triggerHandler(), http.MethodPost, "/api/advisor-trigger", "127.0.0.1:8090",
			map[string]string{"Sec-Fetch-Site": "same-site"})
		wantCode(t, rec, http.StatusForbidden, "別サブドメインを通している")
	})

	t.Run("same-origin(ダッシュボード自身)は通す", func(t *testing.T) {
		rec := serve(triggerHandler(), http.MethodPost, "/api/advisor-trigger", "127.0.0.1:8090",
			map[string]string{"Sec-Fetch-Site": "same-origin"})
		wantCode(t, rec, http.StatusAccepted, "自分の SPA を塞いではいけない")
	})

	t.Run("ヘッダ無し(curl / スクリプト)は通す", func(t *testing.T) {
		rec := serve(triggerHandler(), http.MethodPost, "/api/advisor-trigger", "", nil)
		wantCode(t, rec, http.StatusAccepted, "運用の curl 手順を壊してはいけない")
	})
}

// Sec-Fetch-Site を送らないエンジン(Chromium<76 を同梱した Electron / 旧 WebView)
// では cross-site 判定が空文字で素通りする。ブラウザは POST に必ず Origin を付ける
// ので、そちらも同じ基準で検証しないとゲートに穴が残る。
// curl / 運用スクリプトは Origin を送らないので影響しない。
func TestMutationRejectsCrossOriginWithoutFetchMetadata(t *testing.T) {
	for _, p := range mutatingPaths {
		t.Run("Origin だけ送る cross-origin は 403: "+p, func(t *testing.T) {
			rec := serve(triggerHandler(), http.MethodPost, p, "127.0.0.1:8090",
				map[string]string{"Origin": "https://evil.example"}) // Sec-Fetch-Site は無し
			wantCode(t, rec, http.StatusForbidden, "Origin 未検証で CSRF が通っている")
		})
	}

	for _, host := range []string{"127.0.0.1:8090", "localhost:8090"} {
		t.Run("自分の SPA の same-origin POST は通す: "+host, func(t *testing.T) {
			rec := serve(triggerHandler(), http.MethodPost, "/api/advisor-trigger", host,
				map[string]string{"Origin": "http://" + host, "Sec-Fetch-Site": "same-origin"})
			wantCode(t, rec, http.StatusAccepted, "正規ダッシュボードを塞いではいけない")
		})
	}
}

// read エンドポイントは無認証のままだが、rebinding で吸い出せてよい訳ではない。
// 建玉・戦績・LLM 判断は forward 研究の中身そのもの。
func TestReadsRejectRebinding(t *testing.T) {
	reads := []string{"/api/status", "/api/dashboard", "/api/positions", "/api/performance", "/api/advisor-runs"}

	for _, p := range reads {
		t.Run("攻撃者 Host は 403: "+p, func(t *testing.T) {
			// rebinding では same-origin に見える
			rec := serve(readHandler(), http.MethodGet, p, "rebind.attacker.example",
				map[string]string{"Sec-Fetch-Site": "same-origin"})
			wantCode(t, rec, http.StatusForbidden, "rebinding で読み出せている")
		})
		t.Run("cross-origin の Origin は 403: "+p, func(t *testing.T) {
			rec := serve(readHandler(), http.MethodGet, p, "127.0.0.1:8090",
				map[string]string{"Origin": "https://evil.example"})
			wantCode(t, rec, http.StatusForbidden, "cross-origin の読み出しが通っている")
		})
	}

	// curl / 監視スクリプト(ヘッダ無し)と自分の SPA は従来どおり読める。
	// nil 依存でも本体が動くのは advisor-runs(store 無しは空配列を返す契約)なので、
	// 素通し確認はそれで行う。
	t.Run("ヘッダ無し(curl)は塞がない", func(t *testing.T) {
		rec := serve(readHandler(), http.MethodGet, "/api/advisor-runs", "", nil)
		wantCode(t, rec, http.StatusOK, "curl 経由の読み出しを塞いではいけない")
	})

	t.Run("自分の SPA の same-origin 読み出しは通す", func(t *testing.T) {
		rec := serve(readHandler(), http.MethodGet, "/api/advisor-runs", "127.0.0.1:8090",
			map[string]string{"Origin": "http://127.0.0.1:8090", "Sec-Fetch-Site": "same-origin"})
		wantCode(t, rec, http.StatusOK, "正規ダッシュボードの読み出しを塞いではいけない")
	})
}

// DNS rebinding: 攻撃者ドメインを 127.0.0.1 に解決させると、ブラウザから見れば
// same-origin になるので Sec-Fetch-Site では止まらない。ループバック運用時に
// Host が localhost 系でないリクエストを弾くことで塞ぐ。
func TestMutationRejectsForeignHostOnLoopbackBind(t *testing.T) {
	t.Run("攻撃者ドメインの Host は 403", func(t *testing.T) {
		// rebinding では same-origin に見える
		rec := serve(triggerHandler(), http.MethodPost, "/api/advisor-trigger", "rebind.attacker.example",
			map[string]string{"Sec-Fetch-Site": "same-origin"})
		wantCode(t, rec, http.StatusForbidden, "DNS rebinding が通っている")
	})

	for _, host := range []string{"127.0.0.1:8090", "localhost:8090", "[::1]:8090", "localhost"} {
		t.Run("loopback Host は通す: "+host, func(t *testing.T) {
			// ブラウザ経路を実際に通す
			rec := serve(triggerHandler(), http.MethodPost, "/api/advisor-trigger", host,
				map[string]string{"Sec-Fetch-Site": "same-origin"})
			wantCode(t, rec, http.StatusAccepted, "正規のダッシュボード操作を塞いではいけない")
		})
	}

	// 非ループバック bind は token 必須(既存 fail-close)。その構成では Host は
	// LAN のホスト名/IP になるので、Host 検証は適用しない — token が守る。
	t.Run("非 loopback bind + 正しい token なら任意 Host を通す", func(t *testing.T) {
		h := handler.New(nil, nil, nil, nil, nil, nil).
			WithAuth("t0ken", true).
			WithAdvisorTrigger(func() error { return nil }).
			Routes()
		rec := serve(h, http.MethodPost, "/api/advisor-trigger", "stockbot.lan:8090",
			map[string]string{"X-Api-Token": "t0ken"})
		wantCode(t, rec, http.StatusAccepted, "token 運用の非 loopback を壊してはいけない")
	})
}
