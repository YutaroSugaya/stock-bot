package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// v4r10 リファレンス v10:231 の判定式「(予定日 ≧ 当日日付) AND (予定日 != 前回受信値
// (初回は空白として処理))」を純粋関数で固定する。予定日は YYYYMMDD(応答例 "20250531")。
func TestAPIUpdateDue(t *testing.T) {
	today := time.Date(2026, 9, 4, 23, 30, 0, 0, time.FixedZone("JST", 9*3600))
	cases := []struct {
		planned, last string
		want          bool
	}{
		{"20260904", "", true},          // 当日・初回
		{"20260904", "20260904", false}, // 同じ告知は 1 回だけ
		{"20260905", "20260904", true},  // 予定日が動いた
		{"20260903", "", false},         // 昨日
		{"", "", false},                 // 未定
		{"0", "", false},                // 未定(公式サンプルの空値)
		{"garbage", "", false},          // 解釈不能は鳴らさない(fail-close にしない)
		{" 20260910 ", "", true},        // 空白は落とす
	}
	for _, c := range cases {
		if got := APIUpdateDue(c.planned, c.last, today); got != c.want {
			t.Errorf("APIUpdateDue(%q, %q) = %v, want %v", c.planned, c.last, got, c.want)
		}
	}
}

// login 応答の 2 項目を保持し、APIUpdateNotice() で読めること(未 login は空)。
func TestLogin_CapturesAPIUpdateNotice(t *testing.T) {
	key := testRSAKey(t)
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
		enc := func(s string) string { return encryptForTest(t, &key.PublicKey, s) }
		w.Write(sjisEncode(t, map[string]any{
			"p_no": "1", "p_errno": "0", "sCLMID": tachiCLMLoginAck, "sResultCode": "0",
			"sUrlRequest": enc(base + "/req"), "sUrlMaster": enc(base + "/master"), "sUrlPrice": enc(base + "/price"),
			"sUrlEvent": "", "sZyoutoekiKazeiC": "1", "sKinsyouhouMidokuFlg": "0",
			"sUpdateInformWebDocument": "20241001", "sUpdateInformAPISpecFunction": "20250531",
		}))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	base = ts.URL

	tb := NewTachibana("demo", "authid", key, "s", false, false, nil)
	tb.authBase = ts.URL + "/auth/"
	tb.SetRateLimit(0, 0)
	if got := tb.APIUpdateNotice(); got != (APIUpdateNotice{}) {
		t.Fatalf("before login the notice must be empty, got %+v", got)
	}
	if err := tb.RefreshToken(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got, want := tb.APIUpdateNotice(), (APIUpdateNotice{APISpecFunction: "20250531", WebDocument: "20241001"}); got != want {
		t.Fatalf("APIUpdateNotice() = %+v, want %+v", got, want)
	}
}
