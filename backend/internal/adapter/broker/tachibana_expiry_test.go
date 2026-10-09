package broker

import (
	"context"
	"testing"
)

// 🛑 多日保有の守りは `sOrderExpireDay` で有限(最大10営業日)。切れると建玉は SL が
// どこにも無い状態で残る。塞ぐ経路は **注文訂正**
// (CLMKabuCorrectOrder)— cancel → 再発注にすると、その間だけ守りが完全に消える。
//
// 一次資料(e-shiten API リファレンス)の訂正注文は**全項目必須**で、変えない項目は
// センチネルを送る: sCondition / sOrderPrice / sOrderSuryou / sGyakusasiPrice は "*"。
// 🛑 sGyakusasiZyouken だけは "*" ではなく "0" — 新規注文で両方 "*" を送って
// 「逆指値条件に誤りがあります」で拒否された実績がある(4751)。
func TestTachibana_ListProtectiveOrders_CarriesExpiry(t *testing.T) {
	tb, _, ts := newMockTachibana(t, false, func(clmid, base string) map[string]any {
		if clmid == tachiCLMOrderList {
			return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0",
				"aOrderList": []map[string]string{
					{"sOrderOrderNumber": "1", "sOrderIssueCode": "4751", "sOrderCurrentSuryou": "100",
						"sOrderYakuzyouStatus": "0", "sOrderGyakusasiOrderType": "2",
						"sOrderOrderSuryou": "100", "sOrderBaibaiKubun": "1",
						"sOrderSikkouDay": "20260813", "sOrderOrderExpireDay": "20260826"},
					{"sOrderOrderNumber": "2", "sOrderIssueCode": "4751", "sOrderCurrentSuryou": "100",
						"sOrderYakuzyouStatus": "0", "sOrderGyakusasiOrderType": "0",
						"sOrderOrderExpireDay": "0"},
					{"sOrderOrderNumber": "3", "sOrderIssueCode": "4751", "sOrderCurrentSuryou": "0",
						"sOrderYakuzyouStatus": "0", "sOrderGyakusasiOrderType": "2"},
				}}
		}
		return defaultRespond(clmid, base)
	})
	defer ts.Close()
	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	got, err := tb.ListProtectiveOrders(ctx, "4751")
	if err != nil {
		t.Fatalf("ListProtectiveOrders: %v", err)
	}
	// 取消済(残数量 0)は板に無いので出さない。
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 — %+v", len(got), got)
	}
	if !got[0].HasStopLeg || got[0].ExpireOn.Format("2006-01-02") != "2026-08-26" {
		t.Errorf("got[0] = %+v, want 逆指値あり・期日 2026-08-26", got[0])
	}
	// 🚨 訂正に要る営業日は**照会から運ぶ**(プロセス内 map は再起動で空になる)。
	if got[0].BrokerRef != "20260813" {
		t.Errorf("BrokerRef = %q, want 20260813(訂正の sEigyouDay)", got[0].BrokerRef)
	}
	if got[0].Quantity != 100 {
		t.Errorf("Quantity = %d, want 100(所有権の照合に使う)", got[0].Quantity)
	}
	// "0" = 当日限り。zero を返し、呼び手が「無期限」と読まないようにする。
	if got[1].HasStopLeg || !got[1].ExpireOn.IsZero() {
		t.Errorf("got[1] = %+v, want 逆指値なし・期日 zero(当日限り)", got[1])
	}
}
