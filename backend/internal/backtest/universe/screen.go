// Package universe screens a candidate symbol set down to the tradable research
// universe. Pure arithmetic over already-fetched daily candles: no orders, no
// API, no config. The two criteria are deliberately separate concerns:
//
//   - **流動性**(売買代金中央値): 板が薄い銘柄は紙執行が再現しないスリッページを
//     生む。資金量と無関係な研究の質の下限なので、資金が増えても緩めない。
//   - **資金適合**(最小単元の建玉金額): 実弾で買えない値がさ株の成績は再現できない
//     (paper の勝ちが実弾では張れない価格帯に集中しうる)。
//     **こちらは資金が増えれば緩む** — 二つを同じ数字に混ぜないこと。
package universe

import (
	"math"
	"sort"
	"time"

	"stockbot/backend/internal/domain/market"
)

// MinLotShares は東証の売買単位。2018年の統一以降、内国普通株式は全銘柄 100 株。
// **これは仮定であって取得値ではない** — 立花の銘柄マスタ(CLMStkGetIssueMstKabu)
// を実装したら実値に差し替えること。単元が 100 でない銘柄が混ざると、その銘柄だけ
// 建玉金額の判定がずれる。
const MinLotShares = 100

// Criteria are the screen thresholds. ゼロ値は「その条件を課さない」。
type Criteria struct {
	// 中央値を使うのは、平均だと1日の異常出来高で薄い銘柄が通ってしまうため。
	MedianTurnoverJPY float64
	MaxLotNotionalJPY float64 // 終値×MinLotShares の上限
	Lookback          int     // 中央値を取る直近セッション数 (0 = 60)
	MinBars           int     // これ未満の履歴は判定不能として落とす (0 = Lookback)
	// TopN keeps only the N most liquid symbols among those that cleared BOTH
	// thresholds (0 = 無制限)。**流動性の下限を「額」ではなく「順位」で決める**ため:
	// 毎朝上位N を取れば薄い銘柄は閾値の決め打ちなしに構造的に入らず、場中の一括取得
	// (120銘柄/リクエスト)の通信量も母数に依らず一定になる。
	// TopN は閾値落ちの銘柄を復活させない(枠も消費させない)。
	TopN int
	// MaxStaleDays rejects a symbol whose LAST bar is more than N days older than
	// the newest bar in the whole dataset (0 = 鮮度を課さない)。
	//
	// 売買代金の中央値は「末尾 Lookback 本」であって「直近 Lookback 営業日」では
	// ないので、売買停止で CSV の更新が止まった銘柄は**停止前の健全な売買代金のまま
	// 順位を保ち、選ばれ続ける**。
	//
	// 基準を「暦の今日」ではなくデータセット中の最新バーに取るのは、この層に時計を
	// 持ち込まないためと、連休でデータセット全体が数日古くなっても銘柄間の相対比較が
	// 壊れないため。データセット全体の古さは時計を持つ呼び出し側が別に判定する。
	MaxStaleDays int
}

// Result is one symbol's verdict. Reason is empty when Passed.
type Result struct {
	Symbol            string
	Bars              int
	LastClose         float64
	MedianTurnoverJPY float64
	LotNotionalJPY    float64
	// Rank is the 1-based liquidity rank among symbols that cleared BOTH thresholds
	// (0 = 閾値で落ちた銘柄)。TopN で切られた銘柄にも順位を残す — 「境界の何位で
	// 落ちたか」が分からないと、後から線の妥当性を検証できない。
	Rank   int
	Passed bool
	Reason string
}

// Screen returns results sorted by symbol so the output is deterministic (map
// iteration order must never affect the universe). **落ちた銘柄も理由つきで返す**
// — 黙って消えると「なぜこの銘柄が居ないのか」を後から追えなくなる。
func Screen(universe map[string][]market.Candle, c Criteria) []Result {
	if c.Lookback <= 0 {
		c.Lookback = 60
	}
	if c.MinBars <= 0 {
		c.MinBars = c.Lookback
	}
	newest := newestBar(universe)
	out := make([]Result, 0, len(universe))
	for sym, d := range universe {
		out = append(out, screenOne(sym, d, c, newest))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	rankByLiquidity(out, c.TopN)
	return out
}

// newestBar is the staleness reference point. Zero when no bar carries a
// timestamp, which disables the check rather than failing every symbol.
func newestBar(universe map[string][]market.Candle) time.Time {
	var newest time.Time
	for _, d := range universe {
		if len(d) == 0 {
			continue
		}
		if t := d[len(d)-1].OpenTime; t.After(newest) {
			newest = t
		}
	}
	return newest
}

// 毎朝走らせるので、同じ CSV から必ず同じユニバースが出ることが要件。タイブレーク
// (売買代金が同値 → 銘柄コード昇順)は**比較関数の中で完結**させてある: 呼び出し側が
// 先に銘柄コードでソートしている前提に頼ると、`Screen` のソートを消したり別経路から
// 呼んだりしただけで、テストが緑のまま選定結果が変わる。
func rankByLiquidity(out []Result, topN int) {
	idx := make([]int, 0, len(out))
	for i := range out {
		if out[i].Passed {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(a, b int) bool {
		ta, tb := out[idx[a]].MedianTurnoverJPY, out[idx[b]].MedianTurnoverJPY
		if ta != tb {
			return ta > tb
		}
		return out[idx[a]].Symbol < out[idx[b]].Symbol
	})
	for rank, i := range idx {
		out[i].Rank = rank + 1
		if topN > 0 && rank >= topN {
			out[i].Passed = false
			out[i].Reason = "below_top_n"
		}
	}
}

func screenOne(sym string, d []market.Candle, c Criteria, newest time.Time) Result {
	r := Result{Symbol: sym, Bars: len(d)}
	if len(d) < c.MinBars {
		r.Reason = "insufficient_history"
		return r
	}
	last := d[len(d)-1]
	// 鮮度を**閾値より先に**見る。停止銘柄は停止前の値で全ての閾値を通ってしまう。
	if c.MaxStaleDays > 0 && !newest.IsZero() && !last.OpenTime.IsZero() &&
		last.OpenTime.Before(newest.AddDate(0, 0, -c.MaxStaleDays)) {
		r.LastClose = last.Close
		r.Reason = "stale"
		return r
	}
	r.LastClose = last.Close
	// NaN / Inf を先に弾く。CSV ローダは ParseFloat のエラーを捨てるので "NaN"/"inf"
	// が値として通り、NaN は**あらゆる比較が false** になるため下の「閾値を割ったら
	// 落とす」判定を全てすり抜けて1位を取る(通す方向の最悪の縮退)。
	if r.LastClose <= 0 || math.IsNaN(r.LastClose) || math.IsInf(r.LastClose, 0) {
		r.Reason = "no_price"
		return r
	}
	r.LotNotionalJPY = r.LastClose * MinLotShares
	r.MedianTurnoverJPY = medianTurnover(d, c.Lookback)
	if math.IsNaN(r.MedianTurnoverJPY) || math.IsInf(r.MedianTurnoverJPY, 0) {
		r.MedianTurnoverJPY = 0
		r.Reason = "no_price"
		return r
	}

	// 判定順は「研究の質 → 資金適合」。理由が2つ重なる銘柄は流動性で落ちたと記録する
	// — 資金が増えたときに再訪すべきは資金適合で落ちた側だけなので、この順序が後の
	// 意思決定を変える。
	if c.MedianTurnoverJPY > 0 && r.MedianTurnoverJPY < c.MedianTurnoverJPY {
		r.Reason = "illiquid"
		return r
	}
	if c.MaxLotNotionalJPY > 0 && r.LotNotionalJPY > c.MaxLotNotionalJPY {
		r.Reason = "lot_too_expensive"
		return r
	}
	r.Passed = true
	return r
}

// 出来高ゼロのバー(売買停止・気配のみ)はそのまま 0 として中央値に含める —
// 除くと「半分の日は止まっていた銘柄」が流動的に見えてしまう。
func medianTurnover(d []market.Candle, n int) float64 {
	if n > len(d) {
		n = len(d)
	}
	if n <= 0 {
		return 0
	}
	vals := make([]float64, 0, n)
	for _, b := range d[len(d)-n:] {
		vals = append(vals, b.Close*b.Volume)
	}
	sort.Float64s(vals)
	if len(vals)%2 == 1 {
		return vals[len(vals)/2]
	}
	return (vals[len(vals)/2-1] + vals[len(vals)/2]) / 2
}

func Passed(rs []Result) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		if r.Passed {
			out = append(out, r.Symbol)
		}
	}
	return out
}
