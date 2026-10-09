package main

import (
	"log/slog"

	"stockbot/backend/internal/app"
)

// tachibanaQuoteBatchSize は立花が 1 リクエストに詰められる銘柄数(申告値)。
// 実効間隔 = 3秒 × ceil(監視銘柄数 / これ)。
// 🛑 リテラルなのは internal/config/symbol_budget_guard_test が regexp で読むため。
// broker.DefaultQuoteBatchSize と一致することは quote_chunks_test が固定する。
const tachibanaQuoteBatchSize = 120

// logQuoteChunks は起動時に **一括フィードのチャンク段差の上限**を記録する。
//
// 🚨 **これは上限であって実測ではない。** ここが数える和集合は全 bundle(日次
// ユニバース + live)だが、実効間隔を決めるランタイム値は
// `BatchQuoteFeed` が narrowing した **armed ∪ held ∪ hot** で、桁が違う。
// 以前はこの上限を「実測」として出していたので、どのトラックも narrowing 有効な
// 構成では**初日から chunks=2〜3 と出て警告まで鳴る**一方、実周期は 3 秒
// (chunks=1)のままだった —— 台帳に「サンプリング周期が変わった日」を誤記させる
// 形だった。
//
// **実測は `warnOnQuoteChunkGrowth`**(`broker.QuoteChunks(brk)` を毎ラウンド見て、
// 値が変わったときだけログに出す)。日付つきで記録するのは**そちら**。
//
// 🛑 監視集合は `armed ∪ held` の **トラック和集合**。トラックを 3 本にしても
// **API 回数は増えない**(同一銘柄は dedup され、paper に口座照会は無い)が、
// 和集合が 120 を超えると **両トラックの時価が 3秒 → 6秒** になる。
//
// 影響は 2 つだけで、**どちらも入口には効かない**(入口は前日確定日足
// だけで決まる):
//
//  1. 出口判定の遅延(3秒 → 6秒)
//  2. **分足記録の密度** = バックテストの入力。「サンプリング周期の混在」に
//     区間が 1 つ増える
func logQuoteChunks(research []*app.SymbolBundle, live *liveTrack, logger *slog.Logger) {
	union := map[string]bool{}
	add := func(bs []*app.SymbolBundle) {
		for _, b := range bs {
			union[b.Symbol] = true
		}
	}
	add(research)
	if live != nil {
		add(live.bundles)
	}
	n := len(union)
	chunks := (n + tachibanaQuoteBatchSize - 1) / tachibanaQuoteBatchSize
	if chunks < 1 {
		chunks = 1
	}
	// 🛑 **Warn は出さない**。ここは narrowing 前の上限なので、鳴っても実周期が
	// 伸びたことにはならない。実測が段を上がったときは warnOnQuoteChunkGrowth が
	// 鳴る(そちらが受入条件 5 の観測点)。
	logger.Info("quote chunk step の**上限**(トラック和集合・narrowing 前)",
		"watched_union_upper_bound", n,
		"batch_size", tachibanaQuoteBatchSize,
		"quote_chunks_upper_bound", chunks,
		"note", "実効間隔を決めるのは armed ∪ held ∪ hot に narrowing した後の値。"+
			"受入条件 5 に書く**実測**は『監視銘柄が一括取得の上限を超えた』のログ(warnOnQuoteChunkGrowth)を見ること")
}
