package broker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// --- 公開鍵認証 test helpers ---

// testRSAKey generates a throwaway RSA keypair for the mock login server.
func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	return k
}

// encryptForTest mimics the server: RSA-OAEP(SHA-256) to the client's public key,
// base64-encoded — the exact shape the adapter must decrypt for the 仮想URL.
func encryptForTest(t *testing.T, pub *rsa.PublicKey, s string) string {
	t.Helper()
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, []byte(s), nil)
	if err != nil {
		t.Fatalf("rsa encrypt: %v", err)
	}
	return base64.StdEncoding.EncodeToString(ct)
}

// --- Shift-JIS helpers mirroring the adapter transport ---

func sjisEncode(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, _, err := transform.Bytes(japanese.ShiftJIS.NewEncoder(), b)
	if err != nil {
		t.Fatalf("sjis encode: %v", err)
	}
	return out
}

func decodeReqQuery(t *testing.T, rawQuery string) map[string]string {
	unesc, err := url.QueryUnescape(rawQuery)
	if err != nil {
		t.Fatalf("unescape: %v", err)
	}
	utf8, _, _ := transform.Bytes(japanese.ShiftJIS.NewDecoder(), []byte(unesc))
	var m map[string]string
	if err := json.Unmarshal(utf8, &m); err != nil {
		t.Fatalf("unmarshal req: %v (body=%s)", err, utf8)
	}
	return m
}

// mockTachibana spins up an httptest server emulating the 立花 endpoints and
// returns a logged-out adapter pointed at it, plus a recorder of seen p_no.
type seen struct {
	mu       sync.Mutex
	pnos     []string
	lastReq  map[string]string // last /req CLM fields
	lastAuth map[string]string // last /auth/ login fields
	// logins は /auth/ を叩いた回数。**再ログインが何回起きたか**を数えるのはここだけ
	// (セッションの張り直しは wire にしか現れない)。
	logins int
}

func (s *seen) loginCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

func newMockTachibana(t *testing.T, ocoVerified bool, respond func(clmid string, base string) map[string]any) (*Tachibana, *seen, *httptest.Server) {
	rec := &seen{}
	key := testRSAKey(t)
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
		m := decodeReqQuery(t, r.URL.RawQuery)
		rec.mu.Lock()
		rec.lastAuth = m
		rec.logins++
		rec.mu.Unlock()
		// 公開鍵認証: virtual URLs come back RSA-OAEP+base64 encrypted to the client key.
		enc := func(s string) string { return encryptForTest(t, &key.PublicKey, s) }
		w.Write(sjisEncode(t, map[string]any{
			"p_no": "1", "p_errno": "0", "sCLMID": tachiCLMLoginAck, "sResultCode": "0",
			"sUrlRequest": enc(base + "/req"), "sUrlMaster": enc(base + "/master"), "sUrlPrice": enc(base + "/price"),
			"sUrlEvent": "", "sZyoutoekiKazeiC": "1", "sKinsyouhouMidokuFlg": "0",
		}))
	})
	handle := func(w http.ResponseWriter, r *http.Request) {
		m := decodeReqQuery(t, r.URL.RawQuery)
		rec.mu.Lock()
		rec.pnos = append(rec.pnos, m["p_no"])
		rec.lastReq = m
		rec.mu.Unlock()
		w.Write(sjisEncode(t, respond(m["sCLMID"], base)))
	}
	mux.HandleFunc("/req", handle)
	mux.HandleFunc("/price", handle)
	ts := httptest.NewServer(mux)
	base = ts.URL

	tb := NewTachibana("demo", "authid", key, "second", ocoVerified, false, nil)
	tb.authBase = ts.URL + "/auth/"
	// The default ceiling would make these suites sleep for real. The default
	// itself is asserted in tachibana_ratelimit_test.go, which installs a fake
	// sleeper instead of opting out.
	tb.SetRateLimit(0, 0)
	return tb, rec, ts
}

func defaultRespond(clmid, base string) map[string]any {
	switch clmid {
	case tachiCLMKanougaku:
		return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "sSummaryGenkabuKaituke": "500000", "sHusokukinHasseiFlg": "0"}
	case tachiCLMGenbutu:
		return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aGenbutuKabuList": []map[string]string{
			{"sUriOrderIssueCode": "7203", "sUriOrderZyoutoekiKazeiC": "1", "sUriOrderZanKabuSuryou": "100", "sUriOrderUritukeKanouSuryou": "100", "sUriOrderGaisanBokaTanka": "2000"},
		}}
	case tachiCLMMarketPrice:
		return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aCLMMfdsMarketPrice": []map[string]string{
			{"sIssueCode": "7203", "pDPP": "2050", "pQBP": "2049", "pQAP": "2051", "pGBP1": "2048", "pGAP1": "2052", "pPRP": "2000"},
		}}
	case tachiCLMMarketPriceHistory:
		return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aCLMMfdsMarketPriceHistory": []map[string]string{
			{"sDate": "20260618", "pDOP": "2000", "pDHP": "2030", "pDLP": "1995", "pDPP": "2020", "pDV": "1100000"},
			{"sDate": "20260617", "pDOP": "1990", "pDHP": "2010", "pDLP": "1980", "pDPP": "2000", "pDV": "1000000"},
		}}
	case tachiCLMOrderList:
		// 公式 v4r9 のレコード項目名(T2 裏取り): 注文番号=sOrderOrderNumber, 注文株数=
		// sOrderOrderSuryou, 注文値段=sOrderOrderPrice, 執行日=sOrderSikkouDay,
		// 約定状態=sOrderYakuzyouStatus(0未/1一部/2全部/3約定中)。
		return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aOrderList": []map[string]string{
			{"sOrderOrderNumber": "ORD1", "sOrderIssueCode": "7203", "sOrderBaibaiKubun": "3", "sOrderOrderSuryou": "100", "sOrderCurrentSuryou": "0", "sOrderYakuzyouStatus": "2", "sOrderOrderPrice": "2000", "sOrderSikkouDay": "20260619"},
			{"sOrderOrderNumber": "ORD2", "sOrderIssueCode": "7203", "sOrderBaibaiKubun": "3", "sOrderOrderSuryou": "100", "sOrderCurrentSuryou": "100", "sOrderYakuzyouStatus": "1", "sOrderOrderPrice": "2000", "sOrderSikkouDay": "20260619"},
		}}
	case tachiCLMOrderListDetail:
		return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "sOrderNumber": "ORD1", "sBaiBaiTesuryo": "55", "sShouhizei": "5", "aYakuzyouSikkouList": []map[string]string{
			{"sYakuzyouSuryou": "60", "sYakuzyouPrice": "2000"},
			{"sYakuzyouSuryou": "40", "sYakuzyouPrice": "2050"},
		}}
	case tachiCLMShinyouTate:
		return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aShinyouTategyokuList": []map[string]string{
			{"sOrderTategyokuNumber": "T1", "sOrderIssueCode": "7203", "sOrderBaibaiKubun": "3", "sOrderTategyokuSuryou": "100", "sOrderTategyokuTanka": "2000"},
		}}
	case tachiCLMHosyoukinRitu:
		return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "sItakuHosyoukinRitu": "55.5"}
	case tachiCLMNewOrder:
		return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "sOrderNumber": "ORD1", "sEigyouDay": "20260619"}
	case tachiCLMCancel:
		return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "sOrderNumber": "ORD1", "sEigyouDay": "20260619"}
	case tachiCLMLogout:
		return map[string]any{"p_errno": "0", "sCLMID": "CLMAuthLogoutAck", "sResultCode": "0"}
	default:
		return map[string]any{"p_errno": "1", "sCLMID": clmid}
	}
}

func TestTachibana_LoginAndReadWrite(t *testing.T) {
	tb, rec, ts := newMockTachibana(t, false, defaultRespond)
	defer ts.Close()
	ctx := context.Background()

	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	if tb.session == nil || tb.session.requestURL == "" {
		t.Fatal("session/virtual URLs not set after login")
	}

	am, err := tb.GetAccountMargin(ctx)
	if err != nil {
		t.Fatalf("GetAccountMargin: %v", err)
	}
	if am.AvailableJPY != 500000 {
		t.Fatalf("available = %v, want 500000", am.AvailableJPY)
	}

	pos, err := tb.GetPositions(ctx)
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(pos) != 1 || pos[0].Symbol != "7203" || pos[0].Quantity != 100 || pos[0].EntryPrice != 2000 {
		t.Fatalf("positions = %+v", pos)
	}

	res, err := tb.PlaceOrder(ctx, order.PlaceOrderRequest{Symbol: "7203", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100})
	if err != nil || !res.Accepted || res.OrderID != "ORD1" {
		t.Fatalf("PlaceOrder = %+v err=%v", res, err)
	}
	// buy side + 現物 + market price in the request
	if rec.lastReq["sBaibaiKubun"] != tachiBaibaiBuy || rec.lastReq["sGenkinShinyouKubun"] != tachiGenbutu || rec.lastReq["sOrderPrice"] != tachiOrderPriceMarket {
		t.Fatalf("new-order request fields wrong: %+v", rec.lastReq)
	}
	if tb.recallEigyou("ORD1") != "20260619" {
		t.Fatalf("eigyouDay not remembered: %q", tb.recallEigyou("ORD1"))
	}

	// p_no strictly increasing across all session-bound calls.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	prev := int64(-1)
	for _, p := range rec.pnos {
		var n int64
		_, _ = fmtSscan(p, &n)
		if n <= prev {
			t.Fatalf("p_no not strictly increasing: %v", rec.pnos)
		}
		prev = n
	}
}

func TestTachibana_OCOFailCloseUntilVerified(t *testing.T) {
	ctx := context.Background()
	in := port.OCOCloseOrderInput{Symbol: "7203", BrokerPositionID: "genbutu:7203:1", Side: order.SideSell, Quantity: 100, TakeProfit: 2200, StopLoss: 1840}

	// not verified → fail-close
	tb, _, ts := newMockTachibana(t, false, defaultRespond)
	defer ts.Close()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := tb.PlaceSettleOCO(ctx, in); err == nil {
		t.Fatal("PlaceSettleOCO must fail-close when not verified")
	}
	if _, _, err := tb.ResolveSettleLegs(ctx, in.BrokerPositionID, in.Symbol); err == nil {
		t.Fatal("ResolveSettleLegs must fail-close when not verified")
	}

	// verified → places the protective order; legs resolve to the same id
	tb2, rec, ts2 := newMockTachibana(t, true, defaultRespond)
	defer ts2.Close()
	if err := tb2.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	id, err := tb2.PlaceSettleOCO(ctx, in)
	if err != nil || id != "ORD1" {
		t.Fatalf("PlaceSettleOCO verified = %q err=%v", id, err)
	}
	// TP present → 通常+逆指値 (OCO), sell side, stop trigger = 1840
	if rec.lastReq["sGyakusasiOrderType"] != tachiGyakusasiDouble || rec.lastReq["sBaibaiKubun"] != tachiBaibaiSell || rec.lastReq["sGyakusasiZyouken"] != "1840" {
		t.Fatalf("protective order fields wrong: %+v", rec.lastReq)
	}
	tp, sl, err := tb2.ResolveSettleLegs(ctx, in.BrokerPositionID, in.Symbol)
	if err != nil || tp != "ORD1" || sl != "ORD1" {
		t.Fatalf("ResolveSettleLegs = %q,%q err=%v", tp, sl, err)
	}
}

func TestTachibana_GetTickerAndKlines(t *testing.T) {
	tb, rec, ts := newMockTachibana(t, false, defaultRespond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}

	tk, err := tb.GetTicker(ctx, "7203")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if tk.Last != 2050 || tk.Bid != 2049 || tk.Ask != 2051 {
		t.Fatalf("ticker = %+v, want last2050/bid2049/ask2051", tk)
	}

	// Daily klines come back oldest-first; the latest close is the last element.
	cs, err := tb.GetKlines(ctx, "7203", port.PeriodDaily, 250)
	if err != nil {
		t.Fatalf("GetKlines: %v", err)
	}
	if len(cs) != 2 || cs[len(cs)-1].Close != 2020 || !cs[0].OpenTime.Before(cs[1].OpenTime) {
		t.Fatalf("klines = %+v (want 2 bars, oldest-first, last close 2020)", cs)
	}
	// 日足履歴の request は sIssueCode(時価スナップショットの sTargetIssueCode とは別・T2 裏取り)。
	rec.mu.Lock()
	histReq := rec.lastReq
	rec.mu.Unlock()
	if histReq["sIssueCode"] != "7203" {
		t.Fatalf("history request must send sIssueCode=7203, got %+v", histReq)
	}
	if _, ok := histReq["sTargetIssueCode"]; ok {
		t.Fatalf("history request must NOT send sTargetIssueCode (that is the price-snapshot field): %+v", histReq)
	}

	// Intraday history is not served — fail loud, not silent.
	if _, err := tb.GetKlines(ctx, "7203", port.Period1m, 10); err == nil {
		t.Fatal("GetKlines must error for non-daily periods")
	}
}

func TestTachibana_GetTickerPreOpenFallback(t *testing.T) {
	// Pre-open: 現値/気配 empty → Last falls back to 前日終値 (pPRP).
	respond := func(clmid, base string) map[string]any {
		if clmid == tachiCLMMarketPrice {
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aCLMMfdsMarketPrice": []map[string]string{
				{"sIssueCode": "7203", "pDPP": "", "pQBP": "-", "pQAP": "*", "pGBP1": "", "pGAP1": "", "pPRP": "1980"},
			}}
		}
		return defaultRespond(clmid, base)
	}
	tb, _, ts := newMockTachibana(t, false, respond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	tk, err := tb.GetTicker(ctx, "7203")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if tk.Last != 1980 {
		t.Fatalf("pre-open Last = %v, want 1980 (前日終値)", tk.Last)
	}
}

func TestTachibana_ResolveExecutionAndActiveOrders(t *testing.T) {
	tb, _, ts := newMockTachibana(t, false, defaultRespond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}

	// ORD1 is fully filled: VWAP (60*2000+40*2050)/100 = 2020; fee 55+5 = 60.
	rex, err := tb.ResolveExecution(ctx, "ORD1")
	if err != nil {
		t.Fatalf("ResolveExecution: %v", err)
	}
	if rex.FilledQuantity != 100 || rex.FilledPrice != 2020 || rex.FeeJPY != 60 {
		t.Fatalf("resolved = %+v, want qty100/vwap2020/fee60", rex)
	}
	if rex.BrokerPositionID != "genbutu:7203:1" {
		t.Fatalf("brokerPositionID = %q, want genbutu:7203:1", rex.BrokerPositionID)
	}

	// Only ORD2 (残数量>0, 照会5) is an active working order.
	act, err := tb.GetActiveOrders(ctx, "7203")
	if err != nil {
		t.Fatalf("GetActiveOrders: %v", err)
	}
	if len(act) != 1 || act[0].OrderID != "ORD2" {
		t.Fatalf("active orders = %+v, want only ORD2", act)
	}

	// Executions expand the fill rows.
	ex, err := tb.GetExecutions(ctx, 100)
	if err != nil {
		t.Fatalf("GetExecutions: %v", err)
	}
	if len(ex) == 0 || ex[0].Symbol != "7203" || ex[0].Side != order.SideBuy {
		t.Fatalf("executions = %+v", ex)
	}
}

func TestTachibana_AccountMarginShortfallGate(t *testing.T) {
	// 不足金 (HusokukinHasseiFlg=1) → zero buying power so entries are blocked.
	respond := func(clmid, base string) map[string]any {
		if clmid == tachiCLMKanougaku {
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "sSummaryGenkabuKaituke": "500000", "sHusokukinHasseiFlg": "1"}
		}
		return defaultRespond(clmid, base)
	}
	tb, _, ts := newMockTachibana(t, false, respond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	am, err := tb.GetAccountMargin(ctx)
	if err != nil {
		t.Fatalf("GetAccountMargin: %v", err)
	}
	if am.AvailableJPY != 0 {
		t.Fatalf("available = %v, want 0 under 不足金", am.AvailableJPY)
	}
}

func TestTachibana_SessionInactiveRetryThenError(t *testing.T) {
	// A persistently dead session (p_errno=2 even after re-login) must surface an
	// explicit auth error, not a silent benign result.
	respond := func(clmid, base string) map[string]any {
		if clmid == tachiCLMKanougaku {
			return map[string]any{"p_errno": "2", "sCLMID": clmid} // always session-inactive
		}
		return defaultRespond(clmid, base)
	}
	tb, _, ts := newMockTachibana(t, false, respond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := tb.GetAccountMargin(ctx); err == nil {
		t.Fatal("expected an explicit error when the session is still inactive after re-login")
	}
}

func TestTachibana_MarginOrdersPositionsAndMaintenance(t *testing.T) {
	tb, rec, ts := newMockTachibana(t, true, defaultRespond) // ocoVerified for PlaceSettleOCO
	defer ts.Close()
	tb.marginEnabled = true
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}

	// 一般信用新規買い → 現金信用区分 "6"; 建玉id は shinyo:<code> を合成。
	res, err := tb.PlaceOrder(ctx, order.PlaceOrderRequest{
		Symbol: "7203", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100, ExecKind: order.ExecMarginGeneral,
	})
	if err != nil || !res.Accepted {
		t.Fatalf("PlaceOrder margin = %+v err=%v", res, err)
	}
	if rec.lastReq["sGenkinShinyouKubun"] != tachiGenkinMarginNew {
		t.Fatalf("new 現金信用区分 = %q, want %q (一般信用新規)", rec.lastReq["sGenkinShinyouKubun"], tachiGenkinMarginNew)
	}
	if res.BrokerPositionID != "shinyo:7203" {
		t.Fatalf("margin bpID = %q, want shinyo:7203", res.BrokerPositionID)
	}

	// 一般信用返済(守り)→ 現金信用区分 "8" + 建日順 "2"(FIFO)。
	if _, err := tb.PlaceSettleOCO(ctx, port.OCOCloseOrderInput{
		Symbol: "7203", BrokerPositionID: "shinyo:7203", Side: order.SideSell, Quantity: 100, StopLoss: 1840, ExecKind: order.ExecMarginGeneral,
	}); err != nil {
		t.Fatalf("PlaceSettleOCO margin: %v", err)
	}
	if rec.lastReq["sGenkinShinyouKubun"] != tachiGenkinMarginExit || rec.lastReq["sTatebiType"] != tachiTatebiByDate {
		t.Fatalf("settle 返済 fields = 現金信用区分 %q / 建日種類 %q, want 8 / 2", rec.lastReq["sGenkinShinyouKubun"], rec.lastReq["sTatebiType"])
	}

	// GetPositions includes the 信用建玉 (so Reconcile sees it).
	pos, err := tb.GetPositions(ctx)
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	var found bool
	for _, p := range pos {
		// 立花の信用建玉一覧は制度/一般を区別して返さない。口座で通るのが制度信用
		// なのでそちらに寄せている。
		if p.BrokerPositionID == "shinyo:7203" && p.ExecKind == order.ExecMarginSystem && p.Quantity == 100 && p.Side == order.SideBuy {
			found = true
		}
	}
	if !found {
		t.Fatalf("信用建玉 not returned by GetPositions: %+v", pos)
	}

	// GetAccountMargin reports the real 維持率 (so the maintenance breaker works).
	am, err := tb.GetAccountMargin(ctx)
	if err != nil {
		t.Fatalf("GetAccountMargin: %v", err)
	}
	if am.MarginRatio < 0.554 || am.MarginRatio > 0.556 {
		t.Fatalf("MarginRatio = %v, want ~0.555 (維持率 55.5%%)", am.MarginRatio)
	}
}

func TestTachibana_EnvHostFailClosed(t *testing.T) {
	// Only the explicit "production" value may select the real host; anything else
	// (incl. unset and the typo "prod") must stay on demo so a misconfiguration
	// cannot silently route real orders.
	cases := map[string]string{
		"production": "kabuka.e-shiten.jp",
		"demo":       "demo-kabuka.e-shiten.jp",
		"prod":       "demo-kabuka.e-shiten.jp",
		"":           "demo-kabuka.e-shiten.jp",
	}
	for env, wantHost := range cases {
		tb := NewTachibana(env, "authid", nil, "s", false, false, nil)
		if tb.host != wantHost {
			t.Fatalf("env %q → host %q, want %q (fail-closed to demo)", env, tb.host, wantHost)
		}
	}
}

func TestTachibana_LoginRejectsKinsyouhouMidoku(t *testing.T) {
	rec := &seen{}
	_ = rec
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(sjisEncode(t, map[string]any{
			"p_no": "1", "p_errno": "0", "sCLMID": tachiCLMLoginAck, "sResultCode": "0",
			"sUrlRequest": base + "/req", "sUrlPrice": base + "/price", "sKinsyouhouMidokuFlg": "1", // 金商法書面未読
		}))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	base = ts.URL
	tb := NewTachibana("demo", "authid", nil, "s", false, false, nil)
	tb.authBase = ts.URL + "/auth/"
	if err := tb.RefreshToken(context.Background()); err == nil {
		t.Fatal("login must fail when 金商法書面 is unread (session must not be created)")
	}
}

// TestTachibana_LoginV4r9PubkeyAuth locks in the 公開鍵認証の契約(v4r9 / v4r10 同一): login sends only
// sAuthId (never sUserId/sPassword) and the encrypted 仮想URL群 are decrypted
// with the private key before the session is installed.
func TestTachibana_LoginV4r9PubkeyAuth(t *testing.T) {
	tb, rec, ts := newMockTachibana(t, false, defaultRespond)
	defer ts.Close()
	if err := tb.RefreshToken(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	rec.mu.Lock()
	auth := rec.lastAuth
	rec.mu.Unlock()
	if auth["sAuthId"] != "authid" {
		t.Fatalf("pubkey login must send sAuthId=authid, got %+v", auth)
	}
	if _, ok := auth["sPassword"]; ok {
		t.Fatalf("pubkey login must NOT send sPassword (公開鍵認証): %+v", auth)
	}
	if _, ok := auth["sUserId"]; ok {
		t.Fatalf("pubkey login must NOT send sUserId (公開鍵認証): %+v", auth)
	}
	// The session must hold the DECRYPTED plaintext virtual URLs, not the ciphertext.
	if tb.session == nil || !strings.HasSuffix(tb.session.requestURL, "/req") {
		t.Fatalf("session requestURL not decrypted: %+v", tb.session)
	}
	if !strings.HasSuffix(tb.session.priceURL, "/price") {
		t.Fatalf("session priceURL not decrypted: %+v", tb.session)
	}
}

// TestTachibana_LoginRejectsCorruptVirtualURL fails closed when a 仮想URL cannot
// be decrypted (wrong key / tampered ciphertext) — no session must be installed.
func TestTachibana_LoginRejectsCorruptVirtualURL(t *testing.T) {
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
		// Encrypt to a DIFFERENT key than the adapter holds → decrypt must fail.
		wrong := testRSAKey(t)
		w.Write(sjisEncode(t, map[string]any{
			"p_no": "1", "p_errno": "0", "sCLMID": tachiCLMLoginAck, "sResultCode": "0",
			"sUrlRequest":          encryptForTest(t, &wrong.PublicKey, base+"/req"),
			"sUrlPrice":            encryptForTest(t, &wrong.PublicKey, base+"/price"),
			"sKinsyouhouMidokuFlg": "0",
		}))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	base = ts.URL
	tb := NewTachibana("demo", "authid", testRSAKey(t), "s", false, false, nil)
	tb.authBase = ts.URL + "/auth/"
	if err := tb.RefreshToken(context.Background()); err == nil {
		t.Fatal("login must fail when a virtual URL cannot be decrypted")
	}
	if tb.session != nil {
		t.Fatal("no session must be installed on decrypt failure")
	}
}

// TestParseTachibanaPrivateKey covers PKCS#1, PKCS#8, and garbage input.
func TestParseTachibanaPrivateKey(t *testing.T) {
	key := testRSAKey(t)

	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	got, err := ParseTachibanaPrivateKey(string(pkcs1))
	if err != nil {
		t.Fatalf("parse PKCS#1: %v", err)
	}
	if got.N.Cmp(key.N) != 0 {
		t.Fatal("parsed PKCS#1 key does not match original")
	}

	p8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8})
	if _, err := ParseTachibanaPrivateKey(string(pkcs8)); err != nil {
		t.Fatalf("parse PKCS#8: %v", err)
	}

	if _, err := ParseTachibanaPrivateKey("-----not a pem-----"); err == nil {
		t.Fatal("expected error on non-PEM input")
	}
}

// fmtSscan is a tiny wrapper so the test does not import strconv just for one use.
func fmtSscan(s string, n *int64) (int, error) {
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		v = v*10 + int64(c-'0')
	}
	*n = v
	return 1, nil
}

// 立花の実機(本番の live-probe)は、レコードが 0 件のリスト項目を
// 空配列ではなく **文字列**("" / "*")で返す。json.Unmarshal がここで落ちると
// 「建玉なし」が取得エラー扱いになり、Reconcile が fail-close して bot が止まる
// (= 平常状態で運用不能)。全リスト項目が空文字列でも空リストとして読めること。
func TestTachibanaEmptyListsEncodedAsString(t *testing.T) {
	respond := func(clmid, base string) map[string]any {
		switch clmid {
		case tachiCLMGenbutu:
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aGenbutuKabuList": ""}
		case tachiCLMShinyouTate:
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aShinyouTategyokuList": "*"}
		case tachiCLMOrderList:
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aOrderList": ""}
		case tachiCLMOrderListDetail:
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "sOrderNumber": "ORD1", "aYakuzyouSikkouList": ""}
		case tachiCLMMarketPriceHistory:
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aCLMMfdsMarketPriceHistory": ""}
		case tachiCLMMarketPrice:
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aCLMMfdsMarketPrice": ""}
		}
		return defaultRespond(clmid, base)
	}
	tb, _, ts := newMockTachibana(t, false, respond)
	defer ts.Close()
	tb.marginEnabled = true
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}

	ps, err := tb.GetPositions(ctx)
	if err != nil {
		t.Fatalf("GetPositions with string-encoded empty lists: %v", err)
	}
	if len(ps) != 0 {
		t.Fatalf("GetPositions = %d, want 0", len(ps))
	}
	if _, err := tb.GetActiveOrders(ctx, "7203"); err != nil {
		t.Fatalf("GetActiveOrders with string-encoded empty list: %v", err)
	}
	if _, err := tb.GetExecutions(ctx, 10); err != nil {
		t.Fatalf("GetExecutions with string-encoded empty list: %v", err)
	}
	// 時価/日足は「行が無い」ので値は返せない。ただし JSON parse エラーではなく
	// 業務エラー(データ無し)として返ること = 呼び手が stale 判定できる。
	if _, err := tb.GetTicker(ctx, "7203"); err == nil {
		t.Fatal("GetTicker with no rows: want error")
	} else if strings.Contains(err.Error(), "cannot unmarshal") {
		t.Fatalf("GetTicker: want business error, got json error: %v", err)
	}
	if _, err := tb.GetKlines(ctx, "7203", port.PeriodDaily, 10); err != nil && strings.Contains(err.Error(), "cannot unmarshal") {
		t.Fatalf("GetKlines: want business error or empty, got json error: %v", err)
	}
}

// 🛑 制度信用の現金信用区分。公式 v4r9 仕様:
//
//	0：現物 / 2：新規(制度信用6ヶ月) / 4：返済(制度信用6ヶ月)
//	6：新規(一般信用6ヶ月) / 8：返済(一般信用6ヶ月)
//
// 実口座では一般信用("6")が拒否され、通るのは制度信用だけだった。
func TestGenkinShinyouKubun_MarginSystem(t *testing.T) {
	for _, c := range []struct {
		name    string
		ek      order.ExecKind
		isClose bool
		want    string
	}{
		{"現物", order.ExecCash, false, "0"},
		{"現物返済", order.ExecCash, true, "0"},
		{"制度信用 新規", order.ExecMarginSystem, false, "2"},
		{"制度信用 返済", order.ExecMarginSystem, true, "4"},
		{"一般信用 新規", order.ExecMarginGeneral, false, "6"},
		{"一般信用 返済", order.ExecMarginGeneral, true, "8"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := genkinShinyouKubun(c.ek, c.isClose); got != c.want {
				t.Errorf("genkinShinyouKubun(%s, close=%v)=%q, want %q", c.ek, c.isClose, got, c.want)
			}
		})
	}
}

// 返済時の建日種類は **信用なら区分を問わず** 建日順(FIFO)。制度信用を足したときに
// 一般信用だけ見ていた分岐が漏れると、返済注文が「指定なし」で飛んで拒否される。
func TestNewOrderFields_TatebiTypeForAnyMarginExit(t *testing.T) {
	tb := NewTachibana("demo", "a", nil, "pw", true, true, nil)
	for _, ek := range []order.ExecKind{order.ExecMarginSystem, order.ExecMarginGeneral} {
		f := tb.newOrderFields("7203", order.SideSell, 100, "0", "0", "", "", ek, true /*isClose*/)
		if f["sTatebiType"] != tachiTatebiByDate {
			t.Errorf("%s: sTatebiType=%q, want %q(建日順)", ek, f["sTatebiType"], tachiTatebiByDate)
		}
	}
	// 新規は「指定なし」。
	f := tb.newOrderFields("7203", order.SideBuy, 100, "0", "0", "", "", order.ExecMarginSystem, false)
	if f["sTatebiType"] != tachiUnspecified {
		t.Errorf("新規: sTatebiType=%q, want %q", f["sTatebiType"], tachiUnspecified)
	}
}

// 🛑 逆指値の「指定なし」センチネルは **項目ごとに違う**。
//
//	sGyakusasiZyouken(逆指値条件)  0：指定なし   ← "*" ではない
//	sGyakusasiPrice  (逆指値値段)  *：指定なし / 0：成行
//
// 両方 "*" を送っていて「逆指値条件に誤りがあります」で拒否された(4751)。
// 公式サンプルは全例 `"sGyakusasiZyouken":"0"` / `"sGyakusasiPrice":"*"`。
func TestNewOrderFields_GyakusasiSentinels(t *testing.T) {
	tb := NewTachibana("demo", "a", nil, "pw", true, true, nil)

	// 逆指値なしの通常注文(エントリーの成行はこれ)。
	f := tb.newOrderFields("7203", order.SideBuy, 100, tachiOrderPriceMarket, tachiGyakusasiNone, "", "", order.ExecMarginSystem, false)
	if f["sGyakusasiZyouken"] != tachiGyakusasiZyoukenNone {
		t.Errorf("sGyakusasiZyouken=%q, want %q(指定なしは 0)", f["sGyakusasiZyouken"], tachiGyakusasiZyoukenNone)
	}
	if f["sGyakusasiPrice"] != tachiUnspecified {
		t.Errorf("sGyakusasiPrice=%q, want %q(指定なしは *)", f["sGyakusasiPrice"], tachiUnspecified)
	}

	// 逆指値ありのときは渡した値がそのまま乗る。
	f = tb.newOrderFields("7203", order.SideSell, 100, tachiOrderPriceMarket, "1", "1450", "0", order.ExecMarginSystem, true)
	if f["sGyakusasiZyouken"] != "1450" {
		t.Errorf("トリガー価格が乗っていない: %q", f["sGyakusasiZyouken"])
	}
	if f["sGyakusasiPrice"] != "0" {
		t.Errorf("逆指値値段が乗っていない: %q", f["sGyakusasiPrice"])
	}
}

// 🛑 新規建玉の BrokerPositionID は**信用なら区分を問わず** shinyo:<code>。
//
// reconcile はこの id で「broker 側の建玉」と「台帳の建玉」を突き合わせる。
// 制度信用を足したときこの分岐だけ一般信用のまま残っていたため、
// 制度信用の注文は id が空で返り、台帳には現物形式の
// `genbutu:<code>:1` が入った。broker の `shinyo:<code>` と一致しないので、bot は
// **自分が建てた玉を「外部の裸玉」として二重に採用**した(建玉 1 に対し台帳 2)。
// 枠(account_max_open_positions: 1)が幻に食われるだけでなく、実際に決済された
// とき台帳が 2 本ぶんの決済を書く = 実弾の記録が壊れる。
func TestPlaceOrder_BrokerPositionIDForAnyMargin(t *testing.T) {
	for _, ek := range []order.ExecKind{order.ExecMarginSystem, order.ExecMarginGeneral} {
		t.Run(string(ek), func(t *testing.T) {
			tb, _, ts := newMockTachibana(t, true, defaultRespond)
			defer ts.Close()
			tb.marginEnabled = true
			ctx := context.Background()
			if err := tb.RefreshToken(ctx); err != nil {
				t.Fatalf("login: %v", err)
			}
			res, err := tb.PlaceOrder(ctx, order.PlaceOrderRequest{
				Symbol: "7203", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100, ExecKind: ek,
			})
			if err != nil || !res.Accepted {
				t.Fatalf("PlaceOrder = %+v err=%v", res, err)
			}
			if res.BrokerPositionID != "shinyo:7203" {
				t.Fatalf("%s: BrokerPositionID=%q, want shinyo:7203 — 建玉一覧と突き合わない id で台帳に入ると二重採用される", ek, res.BrokerPositionID)
			}
		})
	}
	// 現物は建玉一覧側が genbutu:<code>:<課税区分> を作るので、発注側では合成しない。
	tb, _, ts := newMockTachibana(t, true, defaultRespond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	res, err := tb.PlaceOrder(ctx, order.PlaceOrderRequest{
		Symbol: "7203", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100, ExecKind: order.ExecCash,
	})
	if err != nil {
		t.Fatalf("PlaceOrder cash: %v", err)
	}
	if res.BrokerPositionID != "" {
		t.Errorf("現物で BrokerPositionID=%q — 発注側で合成しない", res.BrokerPositionID)
	}
}

// 🛑 守りの注文期日。立花の sOrderExpireDay は「0:当日 / それ以外は YYYYMMDD(10営業日迄)」。
// **当日期限のままだと多日保有の建玉は2日目に裸になる**。守りの脚だけ
// 期日を延ばし、新規建て(成行)と成行返済は当日のままにする — 建て注文が数日
// 生き残ると、寄らなかった翌日に突然約定する。
func TestOrderExpireDay(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	expire := time.Date(2026, 8, 26, 0, 0, 0, 0, jst)

	t.Run("守りの脚は期日を持つ", func(t *testing.T) {
		tb, rec, ts := newMockTachibana(t, true, defaultRespond)
		defer ts.Close()
		tb.marginEnabled = true
		ctx := context.Background()
		if err := tb.RefreshToken(ctx); err != nil {
			t.Fatalf("login: %v", err)
		}
		if _, err := tb.PlaceSettleOCO(ctx, port.OCOCloseOrderInput{
			Symbol: "7203", BrokerPositionID: "shinyo:7203", Side: order.SideSell, Quantity: 100,
			TakeProfit: 2200, StopLoss: 1840, ExecKind: order.ExecMarginSystem, ExpireOn: expire,
		}); err != nil {
			t.Fatalf("PlaceSettleOCO: %v", err)
		}
		if got := rec.lastReq["sOrderExpireDay"]; got != "20260826" {
			t.Fatalf("sOrderExpireDay = %q, want 20260826 — 当日期限だと翌日に守りが消える", got)
		}
	})

	t.Run("期日未指定なら当日", func(t *testing.T) {
		tb, rec, ts := newMockTachibana(t, true, defaultRespond)
		defer ts.Close()
		tb.marginEnabled = true
		ctx := context.Background()
		if err := tb.RefreshToken(ctx); err != nil {
			t.Fatalf("login: %v", err)
		}
		if _, err := tb.PlaceSettleOCO(ctx, port.OCOCloseOrderInput{
			Symbol: "7203", BrokerPositionID: "shinyo:7203", Side: order.SideSell, Quantity: 100,
			StopLoss: 1840, ExecKind: order.ExecMarginSystem,
		}); err != nil {
			t.Fatalf("PlaceSettleOCO: %v", err)
		}
		if got := rec.lastReq["sOrderExpireDay"]; got != "0" {
			t.Fatalf("sOrderExpireDay = %q, want 0(当日)", got)
		}
	})

	t.Run("新規建ては常に当日", func(t *testing.T) {
		tb, rec, ts := newMockTachibana(t, true, defaultRespond)
		defer ts.Close()
		tb.marginEnabled = true
		ctx := context.Background()
		if err := tb.RefreshToken(ctx); err != nil {
			t.Fatalf("login: %v", err)
		}
		if _, err := tb.PlaceOrder(ctx, order.PlaceOrderRequest{
			Symbol: "7203", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100, ExecKind: order.ExecMarginSystem,
		}); err != nil {
			t.Fatalf("PlaceOrder: %v", err)
		}
		if got := rec.lastReq["sOrderExpireDay"]; got != "0" {
			t.Fatalf("新規建ての sOrderExpireDay = %q, want 0 — 建て注文が数日生き残ると翌日に突然約定する", got)
		}
	})
}

// 🚨 立花の信用建玉一覧は **建玉ごとに 1 行**返すが、BrokerPositionID は
// shinyo:<コード> で銘柄単位に潰れる。人間が同じ銘柄に複数建玉を持つと、同じ ID の
// BrokerPosition が複数出て、呼び手(reconcile の brokerByID / entry saga の孤児判定)は
// **最後の 1 行だけ**を見る = 数量も建値も実態とずれる。
// 同一 ID は 1 件に**合算**して返す(数量は合計・建値は数量加重平均)。
func TestAggregateByPositionID_SumsQuantityAndWeightsEntryPrice(t *testing.T) {
	in := []port.BrokerPosition{
		{BrokerPositionID: "shinyo:4704", Symbol: "4704", Side: order.SideBuy, Quantity: 100, EntryPrice: 5000, ExecKind: order.ExecMarginSystem},
		{BrokerPositionID: "shinyo:4704", Symbol: "4704", Side: order.SideBuy, Quantity: 300, EntryPrice: 6000, ExecKind: order.ExecMarginSystem},
		{BrokerPositionID: "shinyo:6758", Symbol: "6758", Side: order.SideBuy, Quantity: 100, EntryPrice: 2000, ExecKind: order.ExecMarginSystem},
	}

	got := aggregateByPositionID(in)

	if len(got) != 2 {
		t.Fatalf("件数 = %d, want 2(同一 ID は 1 件に合算): %+v", len(got), got)
	}
	var p4704 port.BrokerPosition
	for _, g := range got {
		if g.BrokerPositionID == "shinyo:4704" {
			p4704 = g
		}
	}
	if p4704.Quantity != 400 {
		t.Fatalf("数量 = %d, want 400(100 + 300)", p4704.Quantity)
	}
	// (5000*100 + 6000*300) / 400 = 5750
	if p4704.EntryPrice != 5750 {
		t.Fatalf("建値 = %v, want 5750(数量加重平均)", p4704.EntryPrice)
	}
}

// 売り建てと買い建てが同じコードにある場合は**別建玉**として残す(合算すると
// 反対売買になる)。ID が同じでも side が違えば潰さない。
func TestAggregateByPositionID_KeepsOppositeSidesApart(t *testing.T) {
	in := []port.BrokerPosition{
		{BrokerPositionID: "shinyo:4704", Symbol: "4704", Side: order.SideBuy, Quantity: 100, EntryPrice: 5000},
		{BrokerPositionID: "shinyo:4704", Symbol: "4704", Side: order.SideSell, Quantity: 200, EntryPrice: 5100},
	}

	if got := aggregateByPositionID(in); len(got) != 2 {
		t.Fatalf("件数 = %d, want 2(買建と売建は別物): %+v", len(got), got)
	}
}

// 決定論(同じ入力なら同じ順序)。reconcile / 孤児判定が順序に依存しても揺れない。
func TestAggregateByPositionID_IsDeterministic(t *testing.T) {
	in := []port.BrokerPosition{
		{BrokerPositionID: "shinyo:9984", Symbol: "9984", Side: order.SideBuy, Quantity: 100, EntryPrice: 100},
		{BrokerPositionID: "shinyo:4704", Symbol: "4704", Side: order.SideBuy, Quantity: 100, EntryPrice: 100},
		{BrokerPositionID: "shinyo:6758", Symbol: "6758", Side: order.SideBuy, Quantity: 100, EntryPrice: 100},
	}
	first := aggregateByPositionID(in)
	for i := 0; i < 5; i++ {
		if got := aggregateByPositionID(in); !reflect.DeepEqual(got, first) {
			t.Fatalf("順序が揺れた: %+v vs %+v", got, first)
		}
	}
}

// 🛑 「決済側の注文が板にある」と「守られている」は別物。逆指値脚を持たない
// ただの利確指値を守りと数えると、SL の無い建玉が「守りあり」で通る。
// 判別子は注文一覧の sOrderGyakusasiOrderType(0通常 / 1逆指値 / 2通常+逆指値)。
func TestTachibana_GetActiveOrdersReportsStopLeg(t *testing.T) {
	respond := func(clmid, base string) map[string]any {
		if clmid == tachiCLMOrderList {
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aOrderList": []map[string]string{
				{"sOrderOrderNumber": "TP", "sOrderIssueCode": "7203", "sOrderBaibaiKubun": "1", "sOrderOrderSuryou": "100", "sOrderCurrentSuryou": "100", "sOrderYakuzyouStatus": "0", "sOrderOrderPrice": "2200", "sOrderSikkouDay": "20260819", "sOrderGyakusasiOrderType": "0"},
				{"sOrderOrderNumber": "STOP", "sOrderIssueCode": "7203", "sOrderBaibaiKubun": "1", "sOrderOrderSuryou": "100", "sOrderCurrentSuryou": "100", "sOrderYakuzyouStatus": "0", "sOrderOrderPrice": "0", "sOrderSikkouDay": "20260819", "sOrderGyakusasiOrderType": "1"},
				{"sOrderOrderNumber": "OCO", "sOrderIssueCode": "7203", "sOrderBaibaiKubun": "1", "sOrderOrderSuryou": "100", "sOrderCurrentSuryou": "100", "sOrderYakuzyouStatus": "0", "sOrderOrderPrice": "2200", "sOrderSikkouDay": "20260819", "sOrderGyakusasiOrderType": "2"},
			}}
		}
		return defaultRespond(clmid, base)
	}
	tb, _, ts := newMockTachibana(t, false, respond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	orders, err := tb.GetActiveOrders(ctx, "7203")
	if err != nil {
		t.Fatalf("GetActiveOrders: %v", err)
	}
	want := map[string]bool{"TP": false, "STOP": true, "OCO": true}
	if len(orders) != len(want) {
		t.Fatalf("expected %d orders, got %d", len(want), len(orders))
	}
	for _, o := range orders {
		w, ok := want[o.OrderID]
		if !ok {
			t.Fatalf("unexpected order %q", o.OrderID)
		}
		if o.HasStopLeg != w {
			t.Errorf("order %s: HasStopLeg=%v, want %v (逆指値脚の有無を取り違えると裸の建玉が守りありで通る)", o.OrderID, o.HasStopLeg, w)
		}
	}
}

// 注文状態コードの実値が repo に無い(docs は「Status 50 常駐」とだけ書く)。
// 締める前に本番の実注文で確かめるための read-only プローブ。
func TestTachibana_ProbeOrderRowsReturnsRawFields(t *testing.T) {
	respond := func(clmid, base string) map[string]any {
		if clmid == tachiCLMOrderList {
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aOrderList": []map[string]string{
				{"sOrderOrderNumber": "A", "sOrderIssueCode": "7203", "sOrderStatusCode": "50", "sOrderYakuzyouStatus": "0", "sOrderGyakusasiOrderType": "2"},
			}}
		}
		return defaultRespond(clmid, base)
	}
	tb, _, ts := newMockTachibana(t, false, respond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	rows, err := tb.ProbeOrderRows(ctx, "7203")
	if err != nil {
		t.Fatalf("ProbeOrderRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	// 生のまま返すこと(加工すると「何が来ているか」を確かめる用途を果たせない)。
	for _, k := range []string{"sOrderOrderNumber", "sOrderStatusCode", "sOrderYakuzyouStatus", "sOrderGyakusasiOrderType"} {
		if rows[0][k] == "" {
			t.Errorf("raw field %s missing from probe output", k)
		}
	}
}

// 🛑 取消済の注文を「有効」と数えてはいけない。死んだ逆指値を守りと読むと、裸の
// 建玉が entry ゲート(protectiveOrderIsResting)も reconcile の trip も素通りする。
//
// 実測(本番 4704): 取消済は sOrderStatus=取消完了 / sOrderStatusCode=7 /
// **残数量 0**、有効は 未約定 / 1 / 残数量 100。両者とも約定状態は "0"(未約定)なので、
// 約定状態だけで判定すると区別がつかない。**残数量が決め手**。
func TestTachibana_GetActiveOrdersExcludesCancelled(t *testing.T) {
	respond := func(clmid, base string) map[string]any {
		if clmid == tachiCLMOrderList {
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aOrderList": []map[string]string{
				{"sOrderOrderNumber": "WORKING", "sOrderIssueCode": "4704", "sOrderBaibaiKubun": "1", "sOrderOrderSuryou": "100", "sOrderCurrentSuryou": "100", "sOrderYakuzyouStatus": "0", "sOrderStatusCode": "1", "sOrderOrderPrice": "6437", "sOrderGyakusasiOrderType": "2"},
				{"sOrderOrderNumber": "CANCELLED", "sOrderIssueCode": "4704", "sOrderBaibaiKubun": "1", "sOrderOrderSuryou": "100", "sOrderCurrentSuryou": "0", "sOrderYakuzyouStatus": "0", "sOrderStatusCode": "7", "sOrderOrderPrice": "0", "sOrderGyakusasiOrderType": "1"},
			}}
		}
		return defaultRespond(clmid, base)
	}
	tb, _, ts := newMockTachibana(t, false, respond)
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	orders, err := tb.GetActiveOrders(ctx, "4704")
	if err != nil {
		t.Fatalf("GetActiveOrders: %v", err)
	}
	if len(orders) != 1 || orders[0].OrderID != "WORKING" {
		t.Fatalf("取消済(残数量0)を有効と数えてはいけない: got %+v", orders)
	}
}

// 🚨 **live で実際に拒否された電文の形を固定する**。
//
// live トラックが呼値 0.1 の銘柄を trail アームで建てた直後、
// `PlaceSettleOCO` が「逆指値条件に誤りがあります」で拒否され、約定済みの建玉が
// 巻き戻った。原因は **domain の呼値丸めが 1 ULP ずれた float を返し、ftoa が
// それを 17 桁の文字列として書き出していた**こと(market.roundTo)。
//
// 🛑 ここで固定するのは「**呼値 0.1 の建玉でも sGyakusasiZyouken が十進で短いまま**」
// という adapter 側の観測。domain のテストは float を見るが、broker が読むのは文字列で、
// その境界を跨いで初めて壊れるバグだったのでこちら側にも検体を置く。
//
// 🛑 stop-only('1')の形そのものは**本番で受理されている**(同じ形の逆指値が
// 他の建玉で板に常駐していた)。だから sOrderPrice="*" は原因ではない — このテストは
// その形も一緒に固定して、次に同じ文言で拒否されたときに切り分けを繰り返さずに済むようにする。
func TestPlaceSettleOCO_WireShape(t *testing.T) {
	ctx := context.Background()

	t.Run("stop-only(TP なし戦略)= 呼値 0.1 の実測値", func(t *testing.T) {
		tb, rec, ts := newMockTachibana(t, true, defaultRespond)
		defer ts.Close()
		if err := tb.RefreshToken(ctx); err != nil {
			t.Fatalf("login: %v", err)
		}
		// 9501 の実際の建値と 2×ATR から domain が出す SL。掛け戻すと 510.20000000000005。
		sl := market.RoundToTickOf("9501", 554.7-44.49999999999998)
		if _, err := tb.PlaceSettleOCO(ctx, port.OCOCloseOrderInput{
			Symbol: "9501", BrokerPositionID: "shinyo:9501", Side: order.SideSell,
			Quantity: 100, TakeProfit: 0, StopLoss: sl, ExecKind: order.ExecMarginSystem,
		}); err != nil {
			t.Fatalf("PlaceSettleOCO: %v", err)
		}
		// 🚨 これが拒否された項目。17 桁で出た瞬間に守りが板に乗らない。
		if got := rec.lastReq["sGyakusasiZyouken"]; got != "510.2" {
			t.Errorf("sGyakusasiZyouken=%q, want %q", got, "510.2")
		}
		if got := rec.lastReq["sGyakusasiOrderType"]; got != tachiGyakusasiStop {
			t.Errorf("sGyakusasiOrderType=%q, want %q(逆指値のみ)", got, tachiGyakusasiStop)
		}
		// TP が無いので通常 leg は「指定なし」、逆指値の執行は成行。
		if got := rec.lastReq["sOrderPrice"]; got != tachiUnspecified {
			t.Errorf("sOrderPrice=%q, want %q(通常 leg なし)", got, tachiUnspecified)
		}
		if got := rec.lastReq["sGyakusasiPrice"]; got != tachiOrderPriceMarket {
			t.Errorf("sGyakusasiPrice=%q, want %q(発動後は成行)", got, tachiOrderPriceMarket)
		}
	})

	t.Run("通常+逆指値(TP あり戦略)= 呼値 0.1", func(t *testing.T) {
		tb, rec, ts := newMockTachibana(t, true, defaultRespond)
		defer ts.Close()
		if err := tb.RefreshToken(ctx); err != nil {
			t.Fatalf("login: %v", err)
		}
		tp := market.RoundToTickOf("9432", 175.66)
		sl := market.RoundToTickOf("9432", 160.04)
		if _, err := tb.PlaceSettleOCO(ctx, port.OCOCloseOrderInput{
			Symbol: "9432", BrokerPositionID: "shinyo:9432", Side: order.SideSell,
			Quantity: 100, TakeProfit: tp, StopLoss: sl, ExecKind: order.ExecMarginSystem,
		}); err != nil {
			t.Fatalf("PlaceSettleOCO: %v", err)
		}
		if got := rec.lastReq["sOrderPrice"]; got != "175.7" {
			t.Errorf("利確指値 sOrderPrice=%q, want %q", got, "175.7")
		}
		if got := rec.lastReq["sGyakusasiZyouken"]; got != "160" {
			t.Errorf("sGyakusasiZyouken=%q, want %q", got, "160")
		}
		if got := rec.lastReq["sGyakusasiOrderType"]; got != tachiGyakusasiDouble {
			t.Errorf("sGyakusasiOrderType=%q, want %q(通常+逆指値)", got, tachiGyakusasiDouble)
		}
	})
}

// 🛑 **ftoa は丸めない。** 呼値グリッドへの丸めは domain の唯一の責務で、adapter が
// 気を利かせて桁を落とすと、格子に載っていない値段(510.23)を黙って 510.2 に
// すり替える fail-open になる — bot が選んでいない値段が板に乗る。
// 上の呼値バグを「ftoa 側で丸める」で塞ぎたくなるが、それは間違いだという記録。
func TestFtoa_DoesNotRound(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{
		{510.2, "510.2"},
		{510.23, "510.23"},                         // 格子外はそのまま出して broker に拒否させる
		{510.20000000000005, "510.20000000000005"}, // 壊れた値も隠さない
		{1568, "1568"},
		{2796, "2796"},
	} {
		if got := ftoa(c.in); got != c.want {
			t.Errorf("ftoa(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
