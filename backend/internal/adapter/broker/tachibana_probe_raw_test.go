package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ProbeRawMaster は MASTER 宛に sCLMID だけを投げ、応答のトップレベルを**解釈せず**返す
// (Q13 の p_errno / p_err、Q11 の配列長、Q7 の語キーを目で確かめる口)。t.request を通るので
// p_no の採番・sJsonOfmt・rate limiter に載る = 「ここを通らない API 経路」を新しく作らない。
func TestProbeRawMaster_ReturnsTopLevelUninterpreted(t *testing.T) {
	var got map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = decodeReqQuery(t, r.URL.RawQuery)
		w.Write(sjisEncode(t, map[string]any{
			"p_no": "2", "p_errno": "0", "p_err": "", "sCLMID": "CLMStkGetIssueSizyouMstKabu",
			"aCLMStkIssueSizyouMstKabu": []map[string]any{{"sIssueCode": "7203"}, {"sIssueCode": "6758"}},
		}))
	}))
	defer ts.Close()

	tb := NewTachibana("demo", "authid", nil, "s", false, false, nil)
	tb.SetRateLimit(0, 0)
	tb.mu.Lock()
	tb.session = &tachiSession{requestURL: ts.URL + "/req", masterURL: ts.URL + "/master", priceURL: ts.URL + "/price", lastNo: 1}
	tb.mu.Unlock()

	raw, err := tb.ProbeRawMaster(context.Background(), "CLMStkGetIssueSizyouMstKabu")
	if err != nil {
		t.Fatalf("ProbeRawMaster: %v", err)
	}
	if got["sCLMID"] != "CLMStkGetIssueSizyouMstKabu" || got["sJsonOfmt"] != tachiJsonOfmt || got["p_no"] == "" {
		t.Fatalf("request must go through the standard envelope (sCLMID / sJsonOfmt / p_no), got %+v", got)
	}
	for _, k := range []string{"p_errno", "p_err", "sCLMID", "aCLMStkIssueSizyouMstKabu"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("top-level key %q missing from raw response %v", k, raw)
		}
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw["aCLMStkIssueSizyouMstKabu"], &rows); err != nil || len(rows) != 2 {
		t.Fatalf("array must be returned raw and countable: err=%v rows=%d", err, len(rows))
	}
}
