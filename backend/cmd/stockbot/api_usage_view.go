package main

import (
	"log/slog"
	"time"

	"stockbot/backend/internal/adapter/apiusage"
)

// dailyTachibanaRequestCap は立花が定める 1 日の利用回数の上限(**bot 側の予算ではなく broker 側の上限**)。
// 内部の dailyQuoteRequestBudget(時価レーンだけの枠)とは別物。
//
// 🛑 実体は `apiusage` に置く。この数を見るのは bot だけではなく、`fetch-daily` も
// 1,550 リクエストを打つ前に残枠として読む。2 か所に literal を
// 書くと、片方だけ直したときに**通信量の判断が静かに食い違う**。
const dailyTachibanaRequestCap = apiusage.BrokerDailyRequestCap

// apiUsageView は dashboard に出す形。立花側の集計(CLMID 単位)と直接見比べられる
// 粒度で出す — 総数だけだと上限を超えたときに「どの処理が原因か」に答えられない。
func apiUsageView(u apiusage.Usage, chunks int) map[string]any {
	if chunks < 1 {
		chunks = 1
	}
	return map[string]any{
		"window_start":          u.WindowStart.Format(time.RFC3339),
		"total":                 u.Total,
		"total_excluding_login": u.TotalExcludingLogin,
		"by_clmid":              u.ByCLMID,
		"cap":                   dailyTachibanaRequestCap,
		// 監視銘柄が 1 リクエストの上限を何倍超えているか。2 以上なら時価の
		// 実効間隔がそのぶん延びている(通信量は一定だが解像度が落ちている)。
		"quote_chunks": chunks,
	}
}

// lastQuoteChunks は直前に警告した段数(同じ状態で15分ごとに鳴り続けないように)。
var lastQuoteChunks int

// warnOnQuoteChunkGrowth は監視銘柄が 1 リクエストの上限を超えたことを一度だけ知らせる。
// **通信量は増えない**(BatchQuoteFeed が実効間隔を chunks 倍にする)が、そのぶん
// 時価の解像度が落ちるので、分足を検定に使う側が気づける必要がある。
func warnOnQuoteChunkGrowth(chunks int, lc loopConfig, logger *slog.Logger) {
	if chunks < 1 {
		chunks = 1
	}
	if chunks == lastQuoteChunks {
		return
	}
	prev := lastQuoteChunks
	lastQuoteChunks = chunks
	if chunks <= 1 {
		if prev > 1 {
			logger.Info("監視銘柄が一括取得の上限内に戻った(時価の間隔も既定へ)",
				"quote_chunks", chunks, "price_interval", lc.priceInterval)
		}
		return
	}
	logger.Warn("監視銘柄が一括取得の上限を超えた — 時価の実効間隔を延ばして通信量を一定に保っている",
		"quote_chunks", chunks,
		"price_interval", lc.priceInterval,
		"effective_interval", lc.priceInterval*time.Duration(chunks),
		"in_session_requests", inSessionQuoteRequests(lc, chunks),
		"note", "1分足のサンプル密度が落ちる。検定に使う期間のメタデータへ記録すること")
}
