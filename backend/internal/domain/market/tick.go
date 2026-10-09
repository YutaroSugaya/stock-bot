// Package market は市場データの値型と rolling aggregator。aggregator は domain で
// 唯一 mutex を許す例外(rolling window は本質的に stateful)。
package market

import (
	"math"
	"time"
)

// Stale = 前日終値 fallback などの indicative 気配。表示・評価には使うが売買判断には使わない
// (price loop は stale tick で evaluate をスキップする)。
type Ticker struct {
	Symbol string
	Bid    float64
	Ask    float64
	Last   float64
	Stale  bool
	// Volume は broker が返す**当日の累計**出来高(株数)。0 = 取れていない。
	//
	// 🛑 これを読んでよいのは**記録の経路だけ**(Aggregator が増分を
	// 積む)。戦略・risk gate・dashboard から読んだ時点で、入口が途中で
	// 変わったことになる。当日出来高を使う戦略は材料が貯まってから
	// 別の入口として事前登録する。
	Volume    float64
	Timestamp time.Time
}

func (t Ticker) Mid() float64 {
	if t.Bid > 0 && t.Ask > 0 {
		return (t.Bid + t.Ask) / 2
	}
	return t.Last
}

// 呼値不明 / 板欠けは 0 を返す(薄商い guard は未知スプレッドを 0 = 非ブロック扱い)。
func (t Ticker) SpreadTicks(tickSize float64) float64 {
	if tickSize <= 0 || t.Bid <= 0 || t.Ask <= 0 {
		return 0
	}
	return (t.Ask - t.Bid) / tickSize
}

// 通常(非細刻み)銘柄の呼値テーブル。銘柄別は TickSizeOf を使う。
func TickSize(price float64) float64 {
	switch {
	case price <= 3000:
		return 1
	case price <= 5000:
		return 5
	case price <= 30000:
		return 10
	case price <= 50000:
		return 50
	case price <= 300000:
		return 100
	case price <= 500000:
		return 500
	case price <= 3000000:
		return 1000
	case price <= 5000000:
		return 5000
	default:
		return 10000
	}
}

// 🚨 **掛け戻さない**。`math.Round(price/ts) * ts` は、ts が二進で
// 表現できない値(呼値では 0.1 だけ)のとき積が 1 ULP ずれる。ずれた float は
// adapter の最短往復表現でそのまま **17 桁の注文値段**として broker へ出て行き、
// 立花は「逆指値条件に誤りがあります」で**注文ごと拒否**する。
// live で呼値 0.1 の銘柄がこれで守りを置けず、約定済みの建玉が
// 巻き戻った(呼値 0.5 の銘柄は同じ stop-only の形で受理されている =
// 原因は形ではなく呼値、という自然実験)。
//
// 🛑 格子の間隔が 1 円未満のときは **逆数で割る**。0.1 の逆数 10 は厳密なので
// n/10 は正しく丸められた十進値になる。ts ≥ 1 は全て整数で積も厳密なので触らない。
// `1/inv == ts` の確認は、逆数を取っても元に戻らない ts(将来テーブルが増えたとき)を
// 従来の経路へ落とすため — 知らない刻みを勝手に割らない。
//
// 🛑 丸めは **ここだけ**でやる(position/price.go)。adapter 側で桁を丸めると、
// 格子に載っていない値段を黙って格子に載っているかのようにすり替える fail-open になる。
func roundTo(price, ts float64) float64 {
	if ts <= 0 {
		return price
	}
	n := math.Round(price / ts)
	if ts < 1 {
		if inv := math.Round(1 / ts); inv > 0 && 1/inv == ts {
			return n / inv
		}
	}
	return n * ts
}

// 呼値グリッド上か。risk gate は非整合の TP/SL を reject する。broker へ送る
// TP/SL は必ずこれ(と RoundToTickOf)を通す。表に無い symbol は粗いテーブルへ倒れる。
func IsTickAlignedOf(symbol string, price float64) bool {
	return isAlignedTo(price, TickSizeOf(symbol, price))
}

func isAlignedTo(price, ts float64) bool {
	if ts <= 0 {
		return true
	}
	// Allow a tiny epsilon for float noise.
	r := price / ts
	return math.Abs(r-math.Round(r)) < 1e-9
}
