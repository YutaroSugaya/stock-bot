package broker

import (
	"context"
	"testing"
)

// 時価API が**どの列を返せるか**を確かめるための read-only プローブ。
// 当日出来高版の前提条件(当日出来高が取れなければ「当日出来高版は永久に不可」と
// 結論する)を、推測ではなく wire で決めるために要る。
func TestTachibana_ProbeMarketPriceColumns(t *testing.T) {
	tb, rec, ts := newMockTachibana(t, false, func(clmid, base string) map[string]any {
		if clmid == tachiCLMMarketPrice {
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0",
				"aCLMMfdsMarketPrice": []map[string]string{
					{"sIssueCode": "7203", "pDPP": "2050", "pDV": "1234500"},
				}}
		}
		return defaultRespond(clmid, base)
	})
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	got, err := tb.ProbeMarketPriceColumns(ctx, "7203", "pDPP,pDV")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	// **生の列をそのまま返す**(解釈しない)。何が返ってきたかを人間が読むための口。
	if got["pDV"] != "1234500" {
		t.Fatalf("pDV = %q, want the raw value 1234500 — got %+v", got["pDV"], got)
	}
	if got["pDPP"] != "2050" {
		t.Fatalf("pDPP = %q, want 2050", got["pDPP"])
	}
	if c := rec.lastReq["sTargetColumn"]; c != "pDPP,pDV" {
		t.Fatalf("sTargetColumn = %q — 要求した列がそのまま wire に載っていない", c)
	}
}

// 列が返らない(未対応)ときは**空で返す**。エラーにしないのは、プローブの目的が
// 「返るか返らないか」の観測そのものだから。
func TestTachibana_ProbeMarketPriceColumns_MissingColumnIsEmpty(t *testing.T) {
	tb, _, ts := newMockTachibana(t, false, func(clmid, base string) map[string]any {
		if clmid == tachiCLMMarketPrice {
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0",
				"aCLMMfdsMarketPrice": []map[string]string{{"sIssueCode": "7203", "pDPP": "2050"}},
			}
		}
		return defaultRespond(clmid, base)
	})
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	got, err := tb.ProbeMarketPriceColumns(ctx, "7203", "pDPP,pDV")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if _, ok := got["pDV"]; ok {
		t.Fatalf("未対応の列は現れないこと: %+v", got)
	}
}
