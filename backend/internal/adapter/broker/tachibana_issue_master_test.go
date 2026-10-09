package broker

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// v4r10 のマスタ経路。v4r9 の `CLMEventDownload`(連結 JSON ストリーム +
// terminator)は廃止されるので、**単発 JSON の個別マスタ問合**へ
// 載せ替える。
//
// 🛑 **完全性の担保は terminator ではなく「下限件数」に置く**。
// v4r10 のマニュアル全文に `p_errno` は 1 件もヒットしないので、封筒に載らない
// 応答を初回で落とすゲートは「本番の失敗直後にゲートを緩めさせる」設計になる。
// だから p_errno は **有るときだけ**見て、完全性そのものは件数で守る。
//
// golden は**本番 v4r10 の実レスポンス**を
// repo に入る大きさへ間引いたもの。
// 項目名は 1 文字も書き換えていない。
// 中身は銘柄マスタ(銘柄コード・値幅・規制フラグ)だけで口座やセッションの項目は無い。
// 立花証券の API 利用規約に従うテスト用の抜粋であり、データの再配布を目的としない。

func loadGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	return b
}

func TestParseIssueMarketMaster_Golden(t *testing.T) {
	var resp masterIssueMarketResp
	if err := json.Unmarshal(loadGolden(t, "CLMStkGetIssueSizyouMstKabu.golden.json"), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Rows) == 0 {
		t.Fatal("行が 0(配列キー aCLMStkIssueSizyouMstKabu が取れていない)")
	}
	var toyota *StockIssueMarket
	for i := range resp.Rows {
		if resp.Rows[i].IssueCode == "7203" {
			toyota = &resp.Rows[i]
		}
	}
	if toyota == nil {
		t.Fatal("golden に 7203 が無い")
	}
	if toyota.ListedMarket == "" || toyota.MarginClass == "" {
		t.Fatalf("7203 の識別キー/値項目が空: %+v", toyota)
	}
}

func TestParseIssueKiseiMaster_Golden(t *testing.T) {
	var resp masterIssueKiseiResp
	if err := json.Unmarshal(loadGolden(t, "CLMStkGetIssueSizyouKiseiKabu.golden.json"), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Rows) == 0 {
		t.Fatal("行が 0(配列キー aCLMStkIssueSizyouKiseiKabu が取れていない)")
	}
	for _, r := range resp.Rows {
		if r.IssueCode == "" || r.ListedMarket == "" {
			t.Fatalf("識別キーが空の行がある: %+v", r)
		}
	}
}

// 🛑 **p_errno は「有るときだけ」見る。** 資料どおりの封筒(p_errno を含まない)を
// 初回で落とすゲートは、運用者に「とりあえず緩める」を強いるだけで安全を足さない。
// 一方 **0 件は無条件で落とす** —— 完全性を担保しているのは件数の方。
func TestGateMasterResp_AbsentErrnoPassesButZeroRowsDoesNot(t *testing.T) {
	if err := gateMasterResp("m", "", 100, 10); err != nil {
		t.Fatalf("p_errno が空なら通すはず: %v", err)
	}
	if err := gateMasterResp("m", "0", 100, 10); err != nil {
		t.Fatalf("p_errno=0 は通すはず: %v", err)
	}
	err := gateMasterResp("m", "", 0, 10)
	if err == nil {
		t.Fatal("0 件は p_errno が空でも落とすはず(0 件を『規制なし』と読むと一覧が広くなる)")
	}
	if !strings.Contains(err.Error(), "0") {
		t.Fatalf("件数が原因だと分かるメッセージにする: %v", err)
	}
}

// p_errno が有って "0" 以外なら error。"1"(データ無し)も落とす —— 静的マスタは
// システム稼働中に更新されないので、空リストが正常な状況が無い。
func TestGateMasterResp_PresentNonZeroErrnoFails(t *testing.T) {
	for _, no := range []string{"1", "2", "-62"} {
		if err := gateMasterResp("m", no, 9999, 10); err == nil {
			t.Fatalf("p_errno=%s を通してはいけない", no)
		}
	}
}

// 下限件数。校正値を下回ったら「途中で切れた一覧」として落とす(fail-close)。
func TestGateMasterResp_BelowFloorFails(t *testing.T) {
	if err := gateMasterResp("m", "", 9, 10); err == nil {
		t.Fatal("下限未満を通してはいけない")
	}
	if err := gateMasterResp("m", "", 10, 10); err != nil {
		t.Fatalf("下限ちょうどは通すはず: %v", err)
	}
}

// 識別キーは **1 行でも空なら** error(行を特定できない = 無いのと同じで、しかも
// 無いことに気づけない)。
func TestRequireMasterKeys_RejectsAnyBlankIdentity(t *testing.T) {
	rows := []StockIssueMarket{{IssueCode: "7203", ListedMarket: "00"}, {IssueCode: "", ListedMarket: "00"}}
	if err := requireIssueMarketKeys(rows); err == nil {
		t.Fatal("識別キーが空の行を通してはいけない")
	}
	ok := []StockIssueMarket{{IssueCode: "7203", ListedMarket: "00"}}
	if err := requireIssueMarketKeys(ok); err != nil {
		t.Fatalf("正常な行を落としてはいけない: %v", err)
	}
}

// 値項目は **全行が空のときだけ** error(項目名が消えた/変わった)。個別行の空値は
// 正規の値であり得る(実測で allowed のうち 72 銘柄が sSinyouC 対象外)。
func TestRequireMasterField_OnlyFailsWhenEveryRowIsBlank(t *testing.T) {
	mixed := []StockIssueMarket{{MarginClass: "1"}, {MarginClass: ""}}
	if err := requireMasterField("sSinyouC", len(mixed), func(i int) string { return mixed[i].MarginClass }); err != nil {
		t.Fatalf("一部が空なのは正常: %v", err)
	}
	blank := []StockIssueMarket{{MarginClass: ""}, {MarginClass: ""}}
	if err := requireMasterField("sSinyouC", len(blank), func(i int) string { return blank[i].MarginClass }); err == nil {
		t.Fatal("全行が空 = 項目名が変わったので落とすはず")
	}
}

// 🛑 commonResp の埋め込みを外すと、p_errno=2 の再ログインが黙って効かなくなる
// (実行時は「再送が消える」だけでテストも緑のまま通る)。型でコンパイルを落とす。
func TestMasterRespCarriesErrNo(t *testing.T) {
	var _ errNoCarrier = (*masterIssueMarketResp)(nil)
	var _ errNoCarrier = (*masterIssueKiseiResp)(nil)
}
