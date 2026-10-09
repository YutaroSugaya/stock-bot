package broker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// batchRespond echoes one row per requested issue code, so the test sees exactly
// what the adapter asked for. recp is read at request time (the harness hands
// back the recorder only after construction).
func batchRespond(recp **seen, priced map[string]string) func(string, string) map[string]any {
	return func(clmid, base string) map[string]any {
		if clmid != tachiCLMMarketPrice {
			return defaultRespond(clmid, base)
		}
		rec := *recp
		rec.mu.Lock()
		req := rec.lastReq["sTargetIssueCode"]
		rec.mu.Unlock()

		rows := []map[string]string{}
		for _, s := range strings.Split(req, ",") {
			p, ok := priced[s]
			if !ok {
				continue // 立花 omits unknown/halted issues rather than erroring
			}
			rows = append(rows, map[string]string{"sIssueCode": s, "pDPP": p, "pQBP": p, "pQAP": p})
		}
		return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0", "aCLMMfdsMarketPrice": rows}
	}
}

// newBatchMock wires the harness so the responder can see the request fields.
func newBatchMock(t *testing.T, priced map[string]string) (*Tachibana, *seen, func()) {
	t.Helper()
	var rec *seen
	tb, r, ts := newMockTachibana(t, false, batchRespond(&rec, priced))
	rec = r
	if err := tb.RefreshToken(context.Background()); err != nil {
		ts.Close()
		t.Fatalf("login: %v", err)
	}
	return tb, rec, ts.Close
}

func TestGetTickersFetchesManySymbolsInOneRequest(t *testing.T) {
	tb, rec, done := newBatchMock(t, map[string]string{"7203": "2900", "6758": "3100", "9984": "8800"})
	defer done()

	before := len(rec.pnos)
	got, err := tb.GetTickers(context.Background(), []string{"7203", "6758", "9984"})
	if err != nil {
		t.Fatalf("GetTickers: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d quotes, want 3: %+v", len(got), got)
	}
	if got["6758"].Last != 3100 {
		t.Errorf("6758 Last=%v, want 3100", got["6758"].Last)
	}
	if calls := len(rec.pnos) - before; calls != 1 {
		t.Fatalf("3銘柄で %d リクエスト — 一括なら 1 回であるべき", calls)
	}
}

// 一括取得の生命線: 応答行は **銘柄コードで厳密に突き合わせる**。単一銘柄用の
// 「該当が無ければ先頭行」フォールバックを持ち込むと、30銘柄要求して1行しか
// 返らなかったとき 29 銘柄に他人の価格を配って紙約定が静かに毒される。
func TestGetTickersNeverSubstitutesAnotherSymbolsPrice(t *testing.T) {
	tb, _, done := newBatchMock(t, map[string]string{"7203": "2900"})
	defer done()

	got, err := tb.GetTickers(context.Background(), []string{"7203", "6758", "9984"})
	if err != nil {
		t.Fatalf("GetTickers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("欠けた銘柄に価格が配られた: %+v", got)
	}
	if _, bad := got["6758"]; bad {
		t.Error("6758 は応答に無いのに価格が付いた")
	}
	if got["7203"].Last != 2900 {
		t.Errorf("7203 Last=%v, want 2900", got["7203"].Last)
	}
}

// 銘柄数が上限を超えたらチャンク分割する(1リクエストに詰めすぎない)。
func TestGetTickersChunksBeyondTheBatchLimit(t *testing.T) {
	priced := map[string]string{}
	syms := make([]string, 0, maxTachibanaQuoteBatch+5)
	for i := 0; i < maxTachibanaQuoteBatch+5; i++ {
		s := fmt.Sprintf("%04d", i)
		syms = append(syms, s)
		priced[s] = "100"
	}
	tb, rec, done := newBatchMock(t, priced)
	defer done()

	before := len(rec.pnos)
	got, err := tb.GetTickers(context.Background(), syms)
	if err != nil {
		t.Fatalf("GetTickers: %v", err)
	}
	if len(got) != len(syms) {
		t.Fatalf("got %d quotes, want %d", len(got), len(syms))
	}
	if calls := len(rec.pnos) - before; calls != 2 {
		t.Fatalf("リクエスト %d 回 — 上限+5 銘柄なら 2 回に分割されるべき", calls)
	}
}

// チャンク上限は可変。50 は立花の上限ではなく自主的な保守値なので、実機で
// 上限を測って引き上げられる必要がある(銘柄が増えても呼び出しを定数に保つ手段)。
func TestSetQuoteBatchSizeChangesChunking(t *testing.T) {
	priced := map[string]string{}
	syms := []string{}
	for i := 0; i < 120; i++ {
		s := fmt.Sprintf("%04d", i)
		syms = append(syms, s)
		priced[s] = "100"
	}
	tb, rec, done := newBatchMock(t, priced)
	defer done()

	tb.SetQuoteBatchSize(120) // 全部を1リクエストに
	before := len(rec.pnos)
	got, err := tb.GetTickers(context.Background(), syms)
	if err != nil {
		t.Fatalf("GetTickers: %v", err)
	}
	if len(got) != 120 {
		t.Fatalf("got %d quotes, want 120", len(got))
	}
	if calls := len(rec.pnos) - before; calls != 1 {
		t.Fatalf("上限120で %d リクエスト — 1 回であるべき", calls)
	}
}

// 壊れた値で「上限なし=1リクエストに全部」になると応答が黙って切り詰められる。
// 非正値は既定に落とす。
func TestSetQuoteBatchSizeRejectsNonPositive(t *testing.T) {
	tb := NewTachibana("demo", "id", testRSAKey(t), "pw", false, false, nil)
	tb.SetQuoteBatchSize(0)
	if tb.quoteBatchSize() != defaultQuoteBatchSize {
		t.Fatalf("batch=%d, want %d", tb.quoteBatchSize(), defaultQuoteBatchSize)
	}
	tb.SetQuoteBatchSize(-5)
	if tb.quoteBatchSize() != defaultQuoteBatchSize {
		t.Fatalf("batch=%d, want %d", tb.quoteBatchSize(), defaultQuoteBatchSize)
	}
}

// 🛑 分割の上限を **BatchQuoteFeed に申告する**こと。申告が無いとフィードは
// 「何銘柄でも1リクエスト」と誤解し、間隔をチャンク数で伸ばす予算保護が本番だけ
// 効かない(テストの fake は申告するので緑のまま)= 最悪の壊れ方をする。
func TestTachibanaDeclaresQuoteBatchSizeToTheFeed(t *testing.T) {
	tb := NewTachibana("demo", "id", testRSAKey(t), "pw", false, false, nil)
	var sizer quoteBatchSizer = tb // コンパイル時に接続を固定する
	if got := sizer.QuoteBatchSize(); got != defaultQuoteBatchSize {
		t.Fatalf("申告 batch=%d, want %d", got, defaultQuoteBatchSize)
	}
	// 実際の分割規則(SetQuoteBatchSize)と申告がずれないこと。
	tb.SetQuoteBatchSize(50)
	if got := sizer.QuoteBatchSize(); got != 50 {
		t.Fatalf("SetQuoteBatchSize(50) 後の申告=%d, want 50 — 申告と分割規則がずれている", got)
	}
}

// 監視銘柄が上限を超えたら、フィードは 1 回の取り直しが何リクエストになるかを
// 正しく数える(ここが 1 のままだと A の予算保護が発動しない)。
func TestBatchQuoteFeedCountsChunksFromTachibanaLimit(t *testing.T) {
	f := newFakeFeed(map[string]float64{})
	f.batchSize = 120
	bf := NewBatchQuoteFeed(f, time.Second, f.clock()).(*BatchQuoteFeed)
	if got := bf.QuoteChunks(); got != 1 {
		t.Fatalf("監視ゼロで chunks=%d, want 1", got)
	}
	bf.mu.Lock()
	for i := 0; i < 121; i++ {
		bf.want[fmt.Sprintf("%d", 1000+i)] = f.now
	}
	bf.mu.Unlock()
	if got := bf.QuoteChunks(); got != 2 {
		t.Fatalf("121銘柄で chunks=%d, want 2", got)
	}
}

// truncatingRespond emulates 立花's REAL behaviour (本番実測):
// 要求銘柄数が上限を超えると、**先頭 cap 件だけ返して残りを黙って落とす**
// (p_errno は 0 のまま・エラーにならない)。
func truncatingRespond(recp **seen, priced map[string]string, cap int) func(string, string) map[string]any {
	inner := batchRespond(recp, priced)
	return func(clmid, base string) map[string]any {
		resp := inner(clmid, base)
		rows, ok := resp["aCLMMfdsMarketPrice"].([]map[string]string)
		if ok && len(rows) > cap {
			resp["aCLMMfdsMarketPrice"] = rows[:cap]
		}
		return resp
	}
}

// 上限超過で末尾が切られても、**残りを取り直して全銘柄を揃える**こと。
// これが無いと、立花が上限を下げた日に監視銘柄の末尾が静かに気配なしになり、
// その銘柄群だけ取引が止まる(エラーが出ないので気付けない)。
func TestGetTickersRecoversFromSilentTailTruncation(t *testing.T) {
	priced := map[string]string{}
	syms := []string{}
	for i := 0; i < 222; i++ {
		s := fmt.Sprintf("%04d", i)
		syms = append(syms, s)
		priced[s] = "100"
	}
	var rec *seen
	tb, r, ts := newMockTachibana(t, false, truncatingRespond(&rec, priced, 120))
	rec = r
	defer ts.Close()
	if err := tb.RefreshToken(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}

	tb.SetQuoteBatchSize(222) // わざと上限超えで投げる
	got, err := tb.GetTickers(context.Background(), syms)
	if err != nil {
		t.Fatalf("GetTickers: %v", err)
	}
	if len(got) != 222 {
		t.Fatalf("切り詰めから回復できていない: %d/222 件", len(got))
	}
}

// 売買停止などで**散らばって**落ちた銘柄は切り詰めではない。取り直さない
// (毎ティック再要求すると呼び出しが増える)。
func TestGetTickersDoesNotRetryScatteredMissingSymbols(t *testing.T) {
	priced := map[string]string{"0000": "100", "0002": "100"} // 0001 だけ気配なし
	tb, rec, done := newBatchMock(t, priced)
	defer done()

	before := len(rec.pnos)
	got, err := tb.GetTickers(context.Background(), []string{"0000", "0001", "0002"})
	if err != nil {
		t.Fatalf("GetTickers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if calls := len(rec.pnos) - before; calls != 1 {
		t.Fatalf("散在する欠けで %d リクエスト — 取り直してはいけない", calls)
	}
}

// 空入力で API を叩かない。
func TestGetTickersEmptyMakesNoCall(t *testing.T) {
	tb, rec, done := newBatchMock(t, nil)
	defer done()

	before := len(rec.pnos)
	got, err := tb.GetTickers(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("GetTickers(nil) = %v, %v", got, err)
	}
	if len(rec.pnos) != before {
		t.Fatal("空入力で API を叩いた")
	}
}

// 寄り前の前日終値フォールバックは単一取得と同じ意味(Stale=true)であること。
func TestGetTickersMarksPrevCloseAsStale(t *testing.T) {
	tb, _, ts := newMockTachibana(t, false, func(clmid, base string) map[string]any {
		if clmid != tachiCLMMarketPrice {
			return defaultRespond(clmid, base)
		}
		return map[string]any{"p_errno": "0", "sCLMID": clmid, "sResultCode": "0",
			"aCLMMfdsMarketPrice": []map[string]string{{"sIssueCode": "7203", "pDPP": "0", "pPRP": "2800"}}}
	})
	defer ts.Close()

	ctx := context.Background()
	if err := tb.RefreshToken(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	got, err := tb.GetTickers(ctx, []string{"7203"})
	if err != nil {
		t.Fatalf("GetTickers: %v", err)
	}
	tk := got["7203"]
	if tk == nil || !tk.Stale || tk.Last != 2800 {
		t.Fatalf("前日終値フォールバックが Stale で返らない: %+v", tk)
	}
}
