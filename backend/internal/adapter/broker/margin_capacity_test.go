package broker

import (
	"context"
	"testing"
)

// 🛑 **信用注文なのに現物の買付余力で判定していた**。
//
// 発注前の collateral ゲートは `CLMZanKaiKanougaku` を読んでいたが、この API が返すのは
// `sSummaryGenkabuKaituke`(**現株**買付可能額)だけで、信用の新規建余力は含まれない。
// 結果、信用で建てられない口座でも事前ガードが素通りし、立花の拒否
// (「新規建余力は0円です。(最低保証金割れ)」)が唯一の防波堤になっていた。
//
// 正しい API は `CLMZanShinkiKanoIjiritu`(建余力＆本日維持率):
//
//	sSummarySinyouSinkidate  信用新規建可能額
//	sItakuhosyoukin          委託保証金率(%)
//	sOisyouKakuteiFlg        追証フラグ    ← 現在どこからも見ていない
func TestGetAccountMargin_UsesMarginCapacityWhenMarginEnabled(t *testing.T) {
	tb, _, srv := newMockTachibana(t, true, func(clmid, _ string) map[string]any {
		switch clmid {
		case tachiCLMKanougaku:
			return map[string]any{"sResultCode": "0", "sSummaryGenkabuKaituke": "300000", "sHusokukinHasseiFlg": "0"}
		case tachiCLMShinkiKanoIjiritu:
			return map[string]any{"sResultCode": "0", "sSummarySinyouSinkidate": "909090",
				"sItakuhosyoukin": "100.00", "sOisyouKakuteiFlg": "0"}
		}
		return map[string]any{"sResultCode": "0"}
	})
	defer srv.Close()
	tb.marginEnabled = true
	if err := tb.RefreshToken(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}

	am, err := tb.GetAccountMargin(context.Background())
	if err != nil {
		t.Fatalf("GetAccountMargin: %v", err)
	}
	if am.MarginNewJPY != 909090 {
		t.Errorf("MarginNewJPY=%v, want 909090(信用新規建可能額)", am.MarginNewJPY)
	}
	if am.AvailableJPY != 300000 {
		t.Errorf("AvailableJPY=%v, want 300000(現株買付は別枠のまま)", am.AvailableJPY)
	}
	if am.MarginRatio != 1.0 {
		t.Errorf("MarginRatio=%v, want 1.0", am.MarginRatio)
	}
}

// 🛑 追証は**発生した時点で新規を止める**。維持率割れの trip とは別軸で、
// 追証中に建て増すのは最悪の行動。
func TestGetAccountMargin_MarginCallStopsNewEntries(t *testing.T) {
	tb, _, srv := newMockTachibana(t, true, func(clmid, _ string) map[string]any {
		switch clmid {
		case tachiCLMKanougaku:
			return map[string]any{"sResultCode": "0", "sSummaryGenkabuKaituke": "300000", "sHusokukinHasseiFlg": "0"}
		case tachiCLMShinkiKanoIjiritu:
			return map[string]any{"sResultCode": "0", "sSummarySinyouSinkidate": "909090",
				"sItakuhosyoukin": "35.00", "sOisyouKakuteiFlg": "1"} // 追証確定
		}
		return map[string]any{"sResultCode": "0"}
	})
	defer srv.Close()
	tb.marginEnabled = true
	if err := tb.RefreshToken(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}

	am, err := tb.GetAccountMargin(context.Background())
	if err != nil {
		t.Fatalf("GetAccountMargin: %v", err)
	}
	if am.MarginNewJPY != 0 {
		t.Errorf("追証中の新規建可能額=%v, want 0(建て増しを許さない)", am.MarginNewJPY)
	}
}
