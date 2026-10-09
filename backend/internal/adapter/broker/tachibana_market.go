package broker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// 時価(CLMMfdsGetMarketPrice)と日足履歴(CLMMfdsGetMarketPriceHistory)。どちらも price 仮想URL。
// 立花は日足しか配信しないので 1m/5m/1h は error にする(黙って誤評価させない。分足は rolling aggregator)。
// ⚠ 要求フィールド名(sTargetIssueCode / sTargetColumn)と応答の配列ラッパは参照実装(v4r7)由来で要デモ確認。
// 応答の**フィールド名**(pDPP/pQBP/…)は裏取り済み。構造(fallback・日足のみ・古い順)は確認に依らず正しい。

// 立花の YYYYMMDD を JST 深夜 = 日足バケットとして読む。
func jstMidnight(ymd string) time.Time {
	t, err := time.ParseInLocation("20060102", ymd, clock.JST)
	if err != nil {
		return time.Time{}
	}
	return t
}

type marketPriceRow struct {
	IssueCode string `json:"sIssueCode"`
	Last      sfloat `json:"pDPP"`  // 現在値
	Bid       sfloat `json:"pQBP"`  // 最良買気配値段
	Ask       sfloat `json:"pQAP"`  // 最良売気配値段
	BidBoard  sfloat `json:"pGBP1"` // 買気配値段1 (Bid 空欄時のフォールバック)
	AskBoard  sfloat `json:"pGAP1"` // 売気配値段1 (Ask 空欄時のフォールバック)
	PrevClose sfloat `json:"pPRP"`  // 前日終値 (寄り前の Last フォールバック)
	Volume    sfloat `json:"pDV"`   // **当日の累計**出来高(株数)。増分に直すのは Aggregator
}

// 時価の要求列。pDV は本番で裏取り(7203 で 22,125,700 株 /
// pDJ 売買代金と整合)。**列を 1 つ増やしてもリクエスト数は変わらない**ので
// API 予算には影響しない。
const tachiQuoteColumns = "pDPP,pQBP,pQAP,pGBP1,pGAP1,pPRP,pDV"

type marketPriceResp struct {
	commonResp
	ResultCode string                `json:"sResultCode"`
	Prices     slist[marketPriceRow] `json:"aCLMMfdsMarketPrice"`
}

// ProbeMarketPriceColumns は時価API に任意の列を要求し、**解釈せず生のまま**返す
// read-only プローブ。仕様書が repo に無いので「この列は返るのか」を推測ではなく
// wire で決めるための口(当日出来高版の前提条件 = 当日出来高が取れるか)。
//
// 返らなかった列は map に現れない。**エラーにしない** — 「返らない」ことの観測が
// このプローブの目的そのものだから。取引経路からは呼ばない(cmd/live-probe 専用)。
func (t *Tachibana) ProbeMarketPriceColumns(ctx context.Context, symbol, columns string) (map[string]string, error) {
	var r struct {
		commonResp
		ResultCode string                   `json:"sResultCode"`
		Prices     slist[map[string]string] `json:"aCLMMfdsMarketPrice"`
	}
	fields := map[string]string{"sTargetIssueCode": symbol, "sTargetColumn": columns}
	if err := t.request(ctx, urlPrice, tachiCLMMarketPrice, fields, &r); err != nil {
		return nil, err
	}
	if err := businessErr("MfdsGetMarketPrice", r.PErrNo, r.ResultCode); err != nil {
		return nil, err
	}
	for _, row := range r.Prices {
		if row["sIssueCode"] == symbol {
			return row, nil
		}
	}
	if len(r.Prices) == 1 {
		return r.Prices[0], nil // 単発要求なら銘柄コードが載らない実装もありうる
	}
	return nil, fmt.Errorf("tachibana: no market price row for %q", symbol)
}

func (t *Tachibana) GetTicker(ctx context.Context, symbol string) (*market.Ticker, error) {
	var r marketPriceResp
	fields := map[string]string{
		"sTargetIssueCode": symbol,
		"sTargetColumn":    tachiQuoteColumns,
	}
	if err := t.request(ctx, urlPrice, tachiCLMMarketPrice, fields, &r); err != nil {
		return nil, err
	}
	if err := businessErr("MfdsGetMarketPrice", r.PErrNo, r.ResultCode); err != nil {
		return nil, err
	}
	row, ok := pickPriceRow(r.Prices, symbol)
	if !ok {
		return nil, fmt.Errorf("tachibana: no market price for %q", symbol)
	}
	tk, ok := t.tickerFromRow(row, symbol)
	if !ok {
		return nil, fmt.Errorf("tachibana: empty quote for %q", symbol)
	}
	return tk, nil
}

// DefaultQuoteBatchSize は 1 リクエストに詰められる銘柄数。120 は仕様値でなく本番実測。
// 詳細: docs/runtime/TACHIBANA_API_NOTES.md §1.5-4。cmd/stockbot の上限ログ用定数は
// これと一致することをテストで固定する(2 箇所に数字があるのは guard test が
// リテラルを regexp で読むため)。
const DefaultQuoteBatchSize = 120

const defaultQuoteBatchSize = DefaultQuoteBatchSize

// 既定値の別名(既存テスト互換)。
const maxTachibanaQuoteBatch = defaultQuoteBatchSize

// 非正値は既定に落とす(「上限なし」にすると黙って切り詰められた分が丸ごと気配なしになる)。
func (t *Tachibana) SetQuoteBatchSize(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n <= 0 {
		n = defaultQuoteBatchSize
	}
	t.batchSize = n
}

// QuoteBatchSize は分割の上限を BatchQuoteFeed へ申告する。フィードはこの値から
// 「1回の取り直しが何リクエストになるか」を数え、間隔をチャンク数だけ伸ばして
// 1日の総リクエスト数を監視銘柄数から切り離す(詳細: batch_feed.go effectiveMaxAge)。
func (t *Tachibana) QuoteBatchSize() int { return t.quoteBatchSize() }

func (t *Tachibana) quoteBatchSize() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.batchSize <= 0 {
		return defaultQuoteBatchSize
	}
	return t.batchSize
}

// 応答に無い銘柄は結果マップに現れない(他銘柄の価格で埋めることは絶対にしない)。詳細: docs/runtime/TACHIBANA_API_NOTES.md §1.5-4
func (t *Tachibana) GetTickers(ctx context.Context, symbols []string) (map[string]*market.Ticker, error) {
	out := make(map[string]*market.Ticker, len(symbols))
	batch := t.quoteBatchSize()
	for start := 0; start < len(symbols); start += batch {
		end := start + batch
		if end > len(symbols) {
			end = len(symbols)
		}
		if err := t.fetchQuoteChunk(ctx, symbols[start:end], out, 0); err != nil {
			return out, err
		}
	}
	return out, nil
}

// 上限が下がり続けるサーバでも回り続けないための打ち切り。
const maxTruncationRetries = 4

// 上限超過はサイレント切り詰め(p_errno=0 のまま末尾が落ちる)なので末尾だけ取り直す。詳細: docs/runtime/TACHIBANA_API_NOTES.md §1.5-4
func (t *Tachibana) fetchQuoteChunk(ctx context.Context, chunk []string, out map[string]*market.Ticker, depth int) error {
	if len(chunk) == 0 {
		return nil
	}
	var r marketPriceResp
	fields := map[string]string{
		"sTargetIssueCode": strings.Join(chunk, ","),
		"sTargetColumn":    tachiQuoteColumns,
	}
	if err := t.request(ctx, urlPrice, tachiCLMMarketPrice, fields, &r); err != nil {
		return err
	}
	if err := businessErr("MfdsGetMarketPrice", r.PErrNo, r.ResultCode); err != nil {
		return err
	}
	want := make(map[string]struct{}, len(chunk))
	for _, s := range chunk {
		want[s] = struct{}{}
	}
	got := make(map[string]struct{}, len(chunk))
	for _, row := range r.Prices {
		// STRICT match: 単一取得の「該当なしなら先頭行」を持ち込むと他銘柄の価格を配って紙約定が毒される。
		if _, ok := want[row.IssueCode]; !ok {
			continue
		}
		got[row.IssueCode] = struct{}{}
		if tk, ok := t.tickerFromRow(row, row.IssueCode); ok {
			out[row.IssueCode] = tk
		}
	}
	if depth >= maxTruncationRetries {
		return nil
	}
	// 末尾が連続して欠けているか = 切り詰めの signature。
	tail := len(chunk)
	for tail > 0 {
		if _, ok := got[chunk[tail-1]]; ok {
			break
		}
		tail--
	}
	if tail == len(chunk) || tail == 0 {
		return nil // 欠けなし、または全滅(=切り詰めではない)
	}
	return t.fetchQuoteChunk(ctx, chunk[tail:], out, depth+1)
}

// 単一取得と一括で意味(前日終値 fallback=Stale・気配のフォールバック順)がずれないよう共有する。
func (t *Tachibana) tickerFromRow(row marketPriceRow, symbol string) (*market.Ticker, bool) {
	last := row.Last.f()
	stale := false
	if last <= 0 {
		last = row.PrevClose.f() // 寄り前/売買停止: 前日終値 = indicative
		stale = true             // これでは評価も約定もしない
	}
	bid := row.Bid.f()
	if bid <= 0 {
		bid = row.BidBoard.f()
	}
	ask := row.Ask.f()
	if ask <= 0 {
		ask = row.AskBoard.f()
	}
	if last <= 0 && bid <= 0 && ask <= 0 {
		return nil, false
	}
	return &market.Ticker{Symbol: symbol, Bid: bid, Ask: ask, Last: last, Stale: stale,
		Volume: row.Volume.f(), Timestamp: t.clock()}, true
}

// 単一取得専用。一括経路には絶対に持ち込まない(先頭行フォールバックが他銘柄に配られる)。
func pickPriceRow(rows []marketPriceRow, symbol string) (marketPriceRow, bool) {
	for _, r := range rows {
		if r.IssueCode == symbol {
			return r, true
		}
	}
	if len(rows) > 0 {
		return rows[0], true
	}
	return marketPriceRow{}, false
}

type historyRow struct {
	Date   string `json:"sDate"` // YYYYMMDD (フィールド名は要デモ確認)
	Open   sfloat `json:"pDOP"`
	High   sfloat `json:"pDHP"`
	Low    sfloat `json:"pDLP"`
	Close  sfloat `json:"pDPP"`
	Volume sfloat `json:"pDV"`
}

type historyResp struct {
	commonResp
	ResultCode string            `json:"sResultCode"`
	Rows       slist[historyRow] `json:"aCLMMfdsMarketPriceHistory"`
}

// 立花は日足しか配信しないので分足は fail-loud(分足は rolling aggregator が作る)。
func (t *Tachibana) GetKlines(ctx context.Context, symbol string, p port.KlinePeriod, n int) ([]market.Candle, error) {
	if p != port.PeriodDaily {
		return nil, fmt.Errorf("tachibana: only daily klines supported (got %q); intraday comes from the aggregator", p)
	}
	var r historyResp
	// 日足履歴は sIssueCode(時価の sTargetIssueCode とは別項目。T2 公式サンプルで裏取り)。
	fields := map[string]string{
		"sIssueCode": symbol,
		"sSizyouC":   tachiSizyouToushou,
	}
	if err := t.request(ctx, urlPrice, tachiCLMMarketPriceHistory, fields, &r); err != nil {
		return nil, err
	}
	if err := businessErr("MfdsGetMarketPriceHistory", r.PErrNo, r.ResultCode); err != nil {
		return nil, err
	}
	out := make([]market.Candle, 0, len(r.Rows))
	for _, row := range r.Rows {
		if row.Close.f() <= 0 { // 休配/欠損行
			continue
		}
		out = append(out, market.Candle{
			Symbol:   symbol,
			Interval: 24 * time.Hour,
			OpenTime: jstMidnight(row.Date),
			Open:     row.Open.f(),
			High:     row.High.f(),
			Low:      row.Low.f(),
			Close:    row.Close.f(),
			Volume:   row.Volume.f(),
		})
	}
	// 古い順。呼び手は out[len-1] を最新として読む。
	sort.SliceStable(out, func(i, j int) bool { return out[i].OpenTime.Before(out[j].OpenTime) })
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}
