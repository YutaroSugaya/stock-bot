package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"stockbot/backend/internal/domain/order"
)

// 立花 v4r9 は JSON ペイロード全体をクエリ文字列に載せ、セッションは仮想 URL の
// パスそのものがトークンになる。Go の http.Client は transport 失敗を *url.Error に
// 包み、その Error() は **クエリ込みの完全 URL** を持つ(標準ライブラリの
// stripPassword は userinfo しか伏せない)。素通しすると、発注中のタイムアウト1回で
// 「セッション URL + 第二パスワード」が同じログ行に平文で落ちる。両方揃えば秘密鍵
// なしで発注できてしまうため、transport エラーは必ず URL を落としてから返す。
func TestDoGETTransportErrorRedactsSessionAndSecondPassword(t *testing.T) {
	const (
		sessionToken = "SESSIONTOKENabcdef0123456789"
		secondPW     = "SecondPW9999"
	)

	// 即 Close したサーバのポートは接続を拒否するので、確実に transport 層で落ちる。
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := ts.URL + "/req/" + sessionToken
	ts.Close()

	tb := NewTachibana("demo", "authid", nil, secondPW, false, false, nil)
	_, err := tb.doGET(context.Background(), dead, map[string]string{
		"sCLMID":          tachiCLMNewOrder,
		"sSecondPassword": secondPW,
	})
	if err == nil {
		t.Fatal("want a transport error from a closed port, got nil")
	}
	got := err.Error()
	if strings.Contains(got, secondPW) {
		t.Errorf("transport error leaks sSecondPassword:\n  %s", got)
	}
	if strings.Contains(got, sessionToken) {
		t.Errorf("transport error leaks the session virtual URL:\n  %s", got)
	}
	// 秘密を落としても「どの CLM が落ちたか」は残さないと運用で追えない。
	if !strings.Contains(got, tachiCLMNewOrder) {
		t.Errorf("transport error dropped the CLM id (unusable for ops):\n  %s", got)
	}
}

// transport エラーだけ塞いでも、非 200 応答の分岐が生 body をエラーに埋めていれば
// 同じ秘密が同じログ行に出る。ゲートウェイのエラーページは要求パスをそのまま echo
// するのが普通で(「/e_api_v4r9/request/<session> not found」)、v4r9 ではその
// パスがセッショントークンそのもの。body は URL より安全ではない。
func TestDoGETNon200DoesNotLeakEchoedSessionURL(t *testing.T) {
	const sessionToken = "SESSIONTOKENabcdef0123456789"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 実ゲートウェイを模して、要求 URL を丸ごと本文に echo する 404。
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "<html><body>The requested URL http://%s%s?%s was not found</body></html>",
			r.Host, r.URL.Path, r.URL.RawQuery)
	}))
	defer ts.Close()

	tb := NewTachibana("demo", "authid", nil, "pw", false, false, nil)
	_, err := tb.doGET(context.Background(), ts.URL+"/req/"+sessionToken, map[string]string{
		"sCLMID": tachiCLMGenbutu,
	})
	if err == nil {
		t.Fatal("want an error on 404, got nil")
	}
	got := err.Error()
	if strings.Contains(got, sessionToken) {
		t.Errorf("non-200 error leaks the echoed session URL:\n  %s", got)
	}
	// ステータスは運用に必要なので残す。
	if !strings.Contains(got, "404") {
		t.Errorf("non-200 error dropped the status code (unusable for ops):\n  %s", got)
	}
}

// 錠は private な doGET ではなく **実際にログへ出る境界** に掛かっていないと意味が
// ない。ログに落ちるのは PlaceOrder / CancelOrder が返す error(loops.go が
// slog "err", err で出す)。発注は第二パスワードを載せる唯一の経路なので、
// その戻り値で直接確かめる。
func TestPlaceOrderTransportErrorDoesNotLeakSecrets(t *testing.T) {
	const (
		sessionToken = "SESSIONTOKENabcdef0123456789"
		secondPW     = "SecondPW9999"
	)

	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := ts.URL
	ts.Close() // ポートが接続を拒否 = 発注中に回線が落ちた状況

	tb := NewTachibana("demo", "authid", nil, secondPW, false, false, nil)
	tb.SetRateLimit(0, 0)
	// ログイン済みセッションを直接注入する(仮想 URL のパス自体がトークン)。
	tb.session = &tachiSession{requestURL: dead + "/req/" + sessionToken}

	_, err := tb.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "7203", Side: order.SideBuy, Quantity: 100, Type: order.OrderTypeMarket,
	})
	if err == nil {
		t.Fatal("want a transport error from a closed port, got nil")
	}
	got := err.Error()
	if strings.Contains(got, secondPW) {
		t.Errorf("PlaceOrder error leaks sSecondPassword:\n  %s", got)
	}
	if strings.Contains(got, sessionToken) {
		t.Errorf("PlaceOrder error leaks the session virtual URL:\n  %s", got)
	}
}

// redact が原因エラーを握り潰さないこと。errors.Is で deadline / cancel を判別できな
// くなると、リトライ判断とシャットダウン検知が壊れる。
func TestDoGETTransportErrorKeepsCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tb := NewTachibana("demo", "authid", nil, "pw", false, false, nil)
	// ポート 1 は接続できない。ctx が先に切れるのでいずれにせよ transport エラー。
	_, err := tb.doGET(ctx, "http://127.0.0.1:1/req/tok", map[string]string{"sCLMID": tachiCLMGenbutu})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("redaction dropped the cause (errors.Is(context.Canceled) failed): %v", err)
	}
}

// 仮想URL のパスに**版 prefix を持たない**トークンを載せた mock で、login 済みの
// adapter を返す。v4r10 マニュアルは仮想URL のフォーマットを保証しない(「意味のない
// 文字列」)ので、`/e_api_.../` を含まない仮想URL は本番で普通に起こり得る。
// newMockTachibana(tachibana_test.go)は仮想URL が常に `/req` `/price` で
// **パスにトークンが乗らない**ため、秘匿の検証には使えない。
func newRedactMock(t *testing.T, token string, handle http.HandlerFunc) *Tachibana {
	t.Helper()
	key := testRSAKey(t)
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
		enc := func(s string) string { return encryptForTest(t, &key.PublicKey, s) }
		w.Write(sjisEncode(t, map[string]any{
			"p_no": "1", "p_errno": "0", "sCLMID": tachiCLMLoginAck, "sResultCode": "0",
			"sUrlRequest": enc(base + "/" + token), "sUrlMaster": enc(base + "/" + token),
			"sUrlPrice": enc(base + "/" + token), "sUrlEvent": "",
			"sUrlEventWebSocket": enc("wss://ws.example.invalid/" + token),
			"sZyoutoekiKazeiC":   "1", "sKinsyouhouMidokuFlg": "0",
		}))
	})
	mux.HandleFunc("/"+token, handle)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	base = ts.URL

	tb := NewTachibana("demo", "authid", key, "SecondPW9999", false, false, nil)
	tb.authBase = ts.URL + "/auth/"
	tb.SetRateLimit(0, 0)
	if err := tb.RefreshToken(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	return tb
}

// 秘匿の正規表現に版番号を焼くと、版を上げた日に**また**セッショントークンが平文で
// ログに残る。v4r9 / v4r10 / その先の版すべてで同じように潰れること。
// 🛑 echo は **scheme 無しの相対パス**にする。絶対URL だと absURLInBody が拾ってしまい、
// 版依存の正規表現が壊れていても気付けない。
func TestRedactBodyIsAPIVersionIndependent(t *testing.T) {
	const tok = "SESSIONTOKENabcdef0123456789"
	for _, ver := range []string{"v4r9", "v4r10", "v5r0", "v99"} {
		t.Run(ver, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, "The requested URL /e_api_%s/request/%s was not found", ver, tok)
			}))
			defer ts.Close()

			tb := NewTachibana("demo", "authid", nil, "pw", false, false, nil)
			_, err := tb.doGET(context.Background(), ts.URL, map[string]string{"sCLMID": tachiCLMGenbutu})
			if err == nil {
				t.Fatal("want an error on 404, got nil")
			}
			if strings.Contains(err.Error(), tok) {
				t.Errorf("non-200 error leaks the echoed session path for %s:\n  %s", ver, err)
			}
		})
	}
}

// 版を焼き直す差し戻しを機械で止める。
func TestRedactPatternCarriesNoHardcodedAPIVersion(t *testing.T) {
	if regexp.MustCompile(`v[0-9]`).MatchString(apiPathInBody.String()) {
		t.Fatalf("api path pattern hardcodes an API version: %s", apiPathInBody.String())
	}
}

// 本文に echo されるのはパスだけではない。ゲートウェイは要求クエリごと echo し、
// クエリには発注時 sSecondPassword が載る(JSON を Shift-JIS + percent-encode しても
// ASCII 英数字はそのまま残る)。scheme が無いので absURLInBody は当たらない。
func TestDoGETNon200DoesNotLeakSecondPassword(t *testing.T) {
	const secondPW = "SecondPW9999"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "upstream rejected: ?%s", r.URL.RawQuery)
	}))
	defer ts.Close()

	tb := NewTachibana("demo", "authid", nil, secondPW, false, false, nil)
	_, err := tb.doGET(context.Background(), ts.URL, map[string]string{
		"sCLMID": tachiCLMNewOrder, "sSecondPassword": secondPW,
	})
	if err == nil {
		t.Fatal("want an error on 502, got nil")
	}
	if strings.Contains(err.Error(), secondPW) {
		t.Errorf("non-200 error leaks sSecondPassword:\n  %s", err)
	}
}

// 錠が掛かっているべきなのは**実際にログへ出る境界**(loops.go が slog "err", err で
// 出す PlaceOrder の戻り値)。仮想URL のフォーマットは無保証なので、版 prefix も
// scheme も持たないパスだけが echo される場合を再現する — 正規表現 2 本はどちらも
// 当たらず、literal 秘匿だけが唯一の防壁になる。
func TestPlaceOrderNon200DoesNotLeakVersionlessVirtualURL(t *testing.T) {
	const tok = "VURLTOKENabcdef0123456789"
	tb := newRedactMock(t, tok, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "<html><body>%s not found</body></html>", r.URL.Path)
	})
	_, err := tb.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "7203", Side: order.SideBuy, Quantity: 100, Type: order.OrderTypeMarket,
	})
	if err == nil {
		t.Fatal("want an error on 404, got nil")
	}
	if strings.Contains(err.Error(), tok) {
		t.Errorf("PlaceOrder error leaks the virtual URL path:\n  %s", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("PlaceOrder error dropped the status code (unusable for ops):\n  %s", err)
	}
}

// 🛑 requestOnce は **t.mu を保持したまま** doGET を呼ぶ。redactBody が t.mu を取る
// 実装(session を読みに行く素朴な設計)にすると、非200 が来た瞬間に永久ハングし、
// ログ秘匿がそのまま bot の停止になる。5 秒で必ず戻ること。
func TestRedactBodyDoesNotDeadlockUnderRequestLock(t *testing.T) {
	const tok = "DEADLOCKTOKENabcdef01234567"
	tb := newRedactMock(t, tok, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "%s not found", r.URL.Path)
	})
	done := make(chan error, 1)
	go func() {
		var out commonResp
		_, err := tb.requestOnce(context.Background(), urlRequest, tachiCLMGenbutu, map[string]string{}, &out)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error on 404, got nil")
		}
		if strings.Contains(err.Error(), tok) {
			t.Errorf("requestOnce error leaks the virtual URL path:\n  %s", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("redactBody deadlocked while requestOnce held t.mu")
	}
}

// login は t.mu を**保持せずに** doGET を呼び、再ログインのたびに literal を差し替える。
// -race で競合が出ないこと。公開済みスライスの backing array を append で使い回す実装
// (next := old[:0])は、ここで DATA RACE として落ちる(実測で確認済)。
//
// 🛑 読み手を回数で止めない。回数だと読みが先に終わって書きと重ならず、-race は何も
// 見ないまま緑になる(実測: 200 回の redactBody は 50 回の login より 2 桁速く終わる)。
// 🛑 in-place ソートはこのテストでは**捕まらない**(一度並べ替えると 2 回目以降は書き込みが
// 起きない)。そちらは TestRedactBodyWithDoesNotMutateCallerSlice が決定論で縛る。
func TestRedactBodyRaceWithLogin(t *testing.T) {
	const tok = "RACETOKENabcdef0123456789"
	tb := newRedactMock(t, tok, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = tb.redactBody([]byte("body /" + tok))
			}
		}
	}()
	for i := 0; i < 50; i++ {
		if err := tb.login(context.Background()); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("login: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// 再ログインで literal を入れ替えた瞬間に前世代を捨てると、**古い**仮想URL を載せた
// 飛行中の要求が非200 で返ったときに平文で残る。パス単体でも登録されること
// (scheme と host を落としてパスだけ echo する 404 では絶対URL の literal が当たらない)。
// 🚨 第 2 パスワードは**何世代 login を重ねても**秘匿対象から落ちない。
// 構築時に 1 回だけ登録されるので FIFO では常に最古 = 上限 32 本を超えた瞬間(毎時の
// 再ログイン 4 回目 ≒ 起動 3 時間後)に押し出され、以後は非 200 の echo で平文になっていた。
func TestRedactKeepsSecondPasswordAcrossManyLogins(t *testing.T) {
	const pw = "SecondPW9999"
	tb := NewTachibana("demo", "authid", nil, pw, false, false, nil)
	for gen := 0; gen < 8; gen++ { // 8 世代 × 10 本 = 上限の 2 倍以上
		lits := make([]string, 0, 5)
		for i := 0; i < 5; i++ {
			lits = append(lits, fmt.Sprintf("http://127.0.0.1:9/gen%d-leg%d-SESSIONTOKEN%02d%02d", gen, i, gen, i))
		}
		tb.rememberRedactLiterals(lits...)
	}

	got := tb.redactBody([]byte("gateway echo: sSecondPassword=" + pw + "&sCLMID=CLMKabuNewOrder"))
	if strings.Contains(got, pw) {
		t.Fatalf("第 2 パスワードが秘匿から押し出された:\n  %s", got)
	}
}

func TestRememberRedactLiteralsKeepsPreviousGeneration(t *testing.T) {
	tb := NewTachibana("demo", "authid", nil, "pw", false, false, nil)
	tb.rememberRedactLiterals("http://127.0.0.1:9/old-SESSIONTOKENaaaa")
	tb.rememberRedactLiterals("http://127.0.0.1:9/new-SESSIONTOKENbbbb")

	got := tb.redactBody([]byte("stale echo /old-SESSIONTOKENaaaa and /new-SESSIONTOKENbbbb"))
	if strings.Contains(got, "SESSIONTOKENaaaa") {
		t.Errorf("re-login dropped the previous session literal:\n  %s", got)
	}
	if strings.Contains(got, "SESSIONTOKENbbbb") {
		t.Errorf("current session literal was not redacted:\n  %s", got)
	}
}

// 🛑 順序: literal が先、正規表現が後。仮想URL は「意味のない文字列」で引用符が混ざり
// 得る。absURLInBody を先に走らせると `https://h/a` までで切れ、**尾が平文で残る**。
func TestRedactBodyRunsLiteralBeforeRegex(t *testing.T) {
	const lit = `https://h/a"b-SECRETTAIL`
	got := redactBodyWith([]string{lit}, []byte("boom "+lit+" end"))
	if strings.Contains(got, "SECRETTAIL") {
		t.Fatalf("regex ran before the literal and left the tail in the clear:\n  %s", got)
	}
}

// 🛑 redactBodyWith は渡されたスライスを**書き換えない**(並べ替えも含む)。引数は
// atomic で publish 済みの共有スライスで、読み手が in-place で並べ替えると書き手
// (rememberRedactLiterals の range)と競合する。
// -race だけでは足りない: 一度並べ替えると 2 回目以降は書き込みが起きないので、
// TestRedactBodyRaceWithLogin は in-place ソートを**見逃す**(実測)。ここは決定論で縛る。
func TestRedactBodyWithDoesNotMutateCallerSlice(t *testing.T) {
	lits := []string{"https://h/abc", "https://h/abc/SESSIONTAIL", "https://h/abcdefgh"}
	before := append([]string(nil), lits...)
	_ = redactBodyWith(lits, []byte("boom https://h/abc/SESSIONTAIL end"))
	for i := range before {
		if lits[i] != before[i] {
			t.Fatalf("redactBodyWith reordered the caller's (published) slice at %d: %v -> %v", i, before, lits)
		}
	}
}

// 🛑 literal 同士は長い順。短い literal(= 仮想URL の prefix)を先に潰すと、長い方が
// 二度と当たらず**尾が平文で残る**。順序は渡した順ではなく redactBodyWith が決めること。
func TestRedactBodyAppliesLongestLiteralFirst(t *testing.T) {
	const short = "https://h/abc"
	const long = short + "/SESSIONTAIL"
	got := redactBodyWith([]string{short, long}, []byte("boom "+long+" end"))
	if strings.Contains(got, "SESSIONTAIL") {
		t.Fatalf("short literal ran first and split the long one:\n  %s", got)
	}
}

// login は sUrlEvent に "" を返すことがある(未契約 leg)。"" を ReplaceAll すると
// 全文字の間に置換文字列が挟まってログが読めなくなる = 運用が止まる。
func TestRedactBodyIgnoresEmptyAndShortLiterals(t *testing.T) {
	const body = "http 404 notfound"
	if got := redactBodyWith([]string{"", "ab"}, []byte(body)); got != body {
		t.Fatalf("short/empty literals corrupted the body: %q", got)
	}
}
