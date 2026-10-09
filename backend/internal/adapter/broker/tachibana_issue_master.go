package broker

import (
	"context"
	"encoding/json"
	"fmt"
)

// v4r10 の**個別マスタ問合**。v4r9 の `CLMEventDownload`(MasterURL へ投げて
// `CLMEventDownload` → 各レコード → `CLMEventDownloadComplete` の連結 JSON が返る)は
// 2026-09-27 に廃止されるので、単発 JSON の問合へ載せ替えた。
//
// 🛑 **読み取り専用。** 貸借銘柄ホワイトリスト(`hard_limits.loanable_symbols`)を
// 人間が commit するための素材を取るためだけにあり、発注系には一切触れない。
// 呼び出し元は `cmd/loanable-fetch` だけで、research / live / harvest のどのトラックにも
// 配線が無い。
//
// 🛑 **`t.request` を通す。** 旧 `streamMaster` は `doGET` を迂回していたので
// **レートリミッタと `p_no` の直列化に載っていなかった**(`ratelimit.go` 冒頭の literal 違反)。
// 新経路は共有 client に戻るのでこの穴が消える。
const (
	clmidIssueMarketMaster = "CLMStkGetIssueSizyouMstKabu"   // 銘柄市場マスタ(信用区分 sSinyouC を持つ)
	clmidIssueKiseiMaster  = "CLMStkGetIssueSizyouKiseiKabu" // 銘柄市場規制(規制のある銘柄だけ載る)
)

// 🛑 **完全性の担保は「下限件数」であって `p_errno` ではない**。
// v4r10 のマニュアル全文に `p_errno` は 1 件もヒットしないので、
// 封筒に載らない応答を初回で落とすゲートは「本番の失敗直後に運用者がゲートを緩める」
// 設計になる。件数で守れば、`p_errno` の意味論が資料から確定できないという問題ごと消える。
//
// 校正値は **v4r10 本番の実測 × 0.9**(本番の実レスポンスを退避して数えた)。
// 実測は 4,444 行 / 546 行。v4r9 の同日の退避物(4,980 / 590)より少ないのは
// **v4r10 が東証の行だけを返す**ため(仕様変更であって欠落ではない)。
//
// ⚠ 規制マスタ側は「規制のある銘柄だけ載る」ので日々動く。下限に触れたら
// **一覧を出さずに落とす**のが正しい(古い一覧を残す = 安全側)。
const (
	masterMinRowsIssueMarket = 3999 // floor(4444 × 0.9)
	masterMinRowsIssueKisei  = 491  // floor(546 × 0.9)
)

// StockIssueMarket は銘柄市場マスタ 1 行。
//
// 🛑 **map[string]string で受けない。** 項目名が変わっても全行 "" で黙って通り、
// 「✅ 貸借銘柄として出す 0 銘柄」に化ける(それを人間が commit すると
// `risk.EvaluateShortLoanable` が全 SELL を fail-close で reject する)。
type StockIssueMarket struct {
	IssueCode    string `json:"sIssueCode"`
	ListedMarket string `json:"sZyouzyouSizyou"`
	MarginClass  string `json:"sSinyouC"` // 信用区分
	PrevClose    string `json:"sZenzituOwarine"`
	TickRule     string `json:"sYobineTaniNumber"`
	DelistDay    string `json:"sZyouzyouHaisiDay"`
}

// StockIssueRegulation は銘柄市場規制 1 行(**規制のある銘柄だけ**載る)。
//
// 🛑 貸借判定に読むのは **`sSeidoSinyou…`(制度信用)**。`sIppanSinyou…`(一般信用)は
// 仕様に区分があるのに口座で拒否された実績があるので、そちらで判定すると
// 「売れると判定したのに発注が拒否される」銘柄が混ざる(CLAUDE.md の実装メモ)。
type StockIssueRegulation struct {
	IssueCode      string `json:"sIssueCode"`
	ListedMarket   string `json:"sZyouzyouSizyou"`
	HaltClass      string `json:"sTeisiKubun"`
	SystemShortNew string `json:"sSeidoSinyouSinkiUritate"` // 制度信用 新規売建("1" = 停止)
}

type masterIssueMarketResp struct {
	commonResp
	Rows []StockIssueMarket `json:"aCLMStkIssueSizyouMstKabu"`
}

type masterIssueKiseiResp struct {
	commonResp
	Rows []StockIssueRegulation `json:"aCLMStkIssueSizyouKiseiKabu"`
}

// 🛑 commonResp の埋め込みを外すと `p_errno=2` の再ログインが黙って効かなくなる
// (実行時は「再送が消える」だけでテストも緑のまま通る)。コンパイルで落とす。
var (
	_ errNoCarrier = (*masterIssueMarketResp)(nil)
	_ errNoCarrier = (*masterIssueKiseiResp)(nil)
)

// FetchStockIssueMarkets は銘柄市場マスタを 1 リクエストで取る。
func (t *Tachibana) FetchStockIssueMarkets(ctx context.Context) ([]StockIssueMarket, error) {
	var resp masterIssueMarketResp
	if err := t.request(ctx, urlMaster, clmidIssueMarketMaster, nil, &resp); err != nil {
		return nil, fmt.Errorf("tachibana %s: %w", clmidIssueMarketMaster, err)
	}
	if err := gateMasterResp(clmidIssueMarketMaster, resp.PErrNo, len(resp.Rows), masterMinRowsIssueMarket); err != nil {
		return nil, err
	}
	if err := requireIssueMarketKeys(resp.Rows); err != nil {
		return nil, err
	}
	if err := requireMasterField("sSinyouC", len(resp.Rows), func(i int) string { return resp.Rows[i].MarginClass }); err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// FetchStockIssueRegulations は銘柄市場規制を 1 リクエストで取る。
func (t *Tachibana) FetchStockIssueRegulations(ctx context.Context) ([]StockIssueRegulation, error) {
	var resp masterIssueKiseiResp
	if err := t.request(ctx, urlMaster, clmidIssueKiseiMaster, nil, &resp); err != nil {
		return nil, fmt.Errorf("tachibana %s: %w", clmidIssueKiseiMaster, err)
	}
	if err := gateMasterResp(clmidIssueKiseiMaster, resp.PErrNo, len(resp.Rows), masterMinRowsIssueKisei); err != nil {
		return nil, err
	}
	for i, r := range resp.Rows {
		if r.IssueCode == "" || r.ListedMarket == "" {
			return nil, fmt.Errorf("tachibana %s: %d 行目の識別キーが空(sIssueCode=%q sZyouzyouSizyou=%q)— 行を特定できない一覧は使わない",
				clmidIssueKiseiMaster, i, r.IssueCode, r.ListedMarket)
		}
	}
	if err := requireMasterField("sSeidoSinyouSinkiUritate", len(resp.Rows), func(i int) string { return resp.Rows[i].SystemShortNew }); err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// gateMasterResp は (b) p_errno と (c) 下限件数。
//
// **p_errno は有るときだけ見る**(空 = 資料どおりの封筒なので通す)。有って "0" 以外は
// error —— "1"(データ無し)も落とす。静的マスタは「システム稼働中、更新されません」と
// 明記されているので、空リストが正常な状況が無い。
//
// **0 件は p_errno に関わらず落とす。** 規制マスタの 0 件を「規制なし」と読むと一覧が
// 広くなり、発注が板で拒否されて同じ往復(約定 → 守り拒否 →
// 補償で決済 → 再入場)を生む。
func gateMasterResp(clmid, errno string, rows, floor int) error {
	if errno != "" && errno != "0" {
		return fmt.Errorf("tachibana %s: p_errno=%s(0 以外は失敗。2=無効セッション / 1=データ無し)— %d 行を全件として扱わない",
			clmid, errno, rows)
	}
	if rows < floor {
		return fmt.Errorf("tachibana %s: %d 行しか返っていない(下限 %d)— 途中で切れた一覧を全件として扱わない",
			clmid, rows, floor)
	}
	return nil
}

// requireIssueMarketKeys は (d) の識別キー側。**1 行でも空なら** error。
// 空の識別キーは「行を特定できない = 無いのと同じ」で、しかも無いことに気づけない。
func requireIssueMarketKeys(rows []StockIssueMarket) error {
	for i, r := range rows {
		if r.IssueCode == "" || r.ListedMarket == "" {
			return fmt.Errorf("tachibana %s: %d 行目の識別キーが空(sIssueCode=%q sZyouzyouSizyou=%q)— 行を特定できない一覧は使わない",
				clmidIssueMarketMaster, i, r.IssueCode, r.ListedMarket)
		}
	}
	return nil
}

// requireMasterField は (d) の値項目側。**全行が空のときだけ** error。
// 個別行の空値は正規の値であり得る(実測で allowed のうち 72 銘柄が sSinyouC 対象外)
// ので、落とすのは「項目名が消えた / 変わった」場合だけ。
func requireMasterField(name string, n int, at func(int) string) error {
	for i := 0; i < n; i++ {
		if at(i) != "" {
			return nil
		}
	}
	return fmt.Errorf("tachibana master: %s が %d 行すべてで空 — 項目名が変わった可能性がある(全行 \"\" で黙って通すと『貸借 0 銘柄』に化ける)", name, n)
}

// ProbeRawMaster は MASTER 宛に {"sCLMID": clmid} だけを投げ、応答の**トップレベルを
// 解釈せずそのまま**返す read-only プローブ。p_errno / p_err / 配列キー / 件数を
// 目で確かめるための口。取引経路からは呼ばない。
//
// 🛑 型付きの Fetch と違って**ゲートを通さない**。仕様の実測用であって、
// ここから貸借一覧を作らない(作ると下限件数ガードを迂回することになる)。
func (t *Tachibana) ProbeRawMaster(ctx context.Context, clmid string) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := t.request(ctx, urlMaster, clmid, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}
