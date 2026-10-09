package broker

import (
	"context"
	"strings"
	"testing"
)

// 時価に**当日の累計出来高**を載せる。本番で裏取り済み
// (7203: pDV=22,125,700 株 / pDJ=66,615,992,650 円 → 平均 3,011 円 ≈ 現在値 3,020 円)。
func TestTachibana_TickerCarriesDailyVolume(t *testing.T) {
	tb, rec, ts := newMockTachibana(t, false, func(clmid, base string) map[string]any {
		if clmid == tachiCLMMarketPrice {
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0",
				"aCLMMfdsMarketPrice": []map[string]string{
					{"sIssueCode": "7203", "pDPP": "3020", "pQBP": "3019", "pQAP": "3021", "pDV": "22125700"},
				}}
		}
		return defaultRespond(clmid, base)
	})
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}

	tk, err := tb.GetTicker(ctx, "7203")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if tk.Volume != 22125700 {
		t.Fatalf("Volume = %v, want 22125700(当日の累計)", tk.Volume)
	}
	if !strings.Contains(rec.lastReq["sTargetColumn"], "pDV") {
		t.Fatalf("sTargetColumn = %q — 出来高列を要求していない", rec.lastReq["sTargetColumn"])
	}
}

// 一括取得(場中に使うのはこちら)でも同じ列を要求すること。単発だけ直すと
// **記録が場中は空のまま**になる。
func TestTachibana_BatchQuotesCarryVolume(t *testing.T) {
	tb, rec, ts := newMockTachibana(t, false, func(clmid, base string) map[string]any {
		if clmid == tachiCLMMarketPrice {
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0",
				"aCLMMfdsMarketPrice": []map[string]string{
					{"sIssueCode": "7203", "pDPP": "3020", "pQBP": "3019", "pQAP": "3021", "pDV": "22125700"},
					{"sIssueCode": "6501", "pDPP": "4000", "pQBP": "3999", "pQAP": "4001", "pDV": "1000"},
				}}
		}
		return defaultRespond(clmid, base)
	})
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}

	got, err := tb.GetTickers(ctx, []string{"7203", "6501"})
	if err != nil {
		t.Fatalf("GetTickers: %v", err)
	}
	if got["7203"].Volume != 22125700 || got["6501"].Volume != 1000 {
		t.Fatalf("一括の Volume = %+v", got)
	}
	if !strings.Contains(rec.lastReq["sTargetColumn"], "pDV") {
		t.Fatalf("一括の sTargetColumn = %q", rec.lastReq["sTargetColumn"])
	}
}
