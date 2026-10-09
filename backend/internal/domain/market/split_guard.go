package market

import (
	"fmt"
	"math"
	"stockbot/backend/internal/domain/clock"
	"time"
)

// 分割検出のしきい値(|log(終値比)|)。0.35 ≈ ±42%/−30%: 値幅制限いっぱいの実相場(ローカル日足177銘柄に
// 実際に33件ある)は通し、最小分割比 1:1.5(log 0.405)以上は捕まえる、という間の値。
// 要る理由: broker(立花)の日足は**分割未調整**で、1:5 分割は見かけ −80% のバー = 逆張り戦略にとって
// 最高のエントリー条件に化ける。取り込む前に捨てる。
const SplitLogMoveThreshold = 0.35

// 検査対象は渡された系列の内部のみ。呼び手は「継ぎ目 + 新規バー」だけを渡すこと
// (全期間を渡すと過去の実暴落 1 件でその銘柄の更新が恒久的に止まる)。
func SplitDiscontinuity(bars []Candle) string {
	for i := 1; i < len(bars); i++ {
		p, c := bars[i-1].Close, bars[i].Close
		if p <= 0 || c <= 0 {
			continue
		}
		if math.Abs(math.Log(c/p)) > SplitLogMoveThreshold {
			return fmt.Sprintf("%s→%s: %.1f→%.1f",
				JSTDayKey(bars[i-1].OpenTime), JSTDayKey(bars[i].OpenTime), p, c)
		}
	}
	return ""
}

// 同一営業日のバーが水準で食い違う = 調整規約の異なる2ソースを混ぜた兆候(Yahoo 調整済み vs broker 未調整)。
func SameDayLevelMismatch(stored, fetched []Candle) string {
	byDay := make(map[string]Candle, len(stored))
	for _, c := range stored {
		byDay[JSTDayKey(c.OpenTime)] = c
	}
	for _, f := range fetched {
		s, ok := byDay[JSTDayKey(f.OpenTime)]
		if !ok || s.Close <= 0 || f.Close <= 0 {
			continue
		}
		if math.Abs(math.Log(f.Close/s.Close)) > SplitLogMoveThreshold {
			return fmt.Sprintf("%s: stored %.1f vs fetched %.1f", JSTDayKey(f.OpenTime), s.Close, f.Close)
		}
	}
	return ""
}

// 実在する分割/併合比。これに一致しない急変は本物の相場なので絶対に均さない(均すと BNF が狙う暴落が消える)。
// 🛑 6 / 7 / 8 は後から追加。5803(1:6.13)と 7013(1:6.67)が候補に無くて
// **検出されず**、CSV 段階で未調整のまま残っていた — 逆張り(BNF)から見ると
// -84% の理想的なパニックに化ける。
// 深い比を足しても実暴落と混同しない根拠は**値幅制限**: ¥1,000 の銘柄で 1 日
// ±¥300(30%)が上限なので、1/6 のような段差は制度上ありえない。
// 🛑 12 / 15 / 25 / 30 / 40 / 50 は後から追加。8766(東京海上・1:15・権利落ち 9/29)が
// 候補に無くて**検出されず**、日足は未調整の段差のまま(selector が除外)、建玉の言い直し
// (ExDateSplitRatio)も効かない状態だった(建玉が無かったので実害は無し)。
var simpleSplitRatios = []float64{1.5, 2, 2.5, 3, 4, 5, 6, 7, 8, 10, 12, 15, 20, 25, 30, 40, 50, 100}

// 分割日当日の値動き分の許容。浅い比は狭く、深い比(≤1/2 / ≥2)は広く
// (実測: 8001 は 1:5 分割当日に +5% 動き 5% 許容を外れた)。
const (
	splitRatioTolerance     = 0.05
	splitRatioToleranceDeep = 0.12
)

// これより小さい下落比だけを分割候補にする。1/1.5 ≈ 0.667 は実暴落と区別できないので下落方向では分割扱いしない。
const downSplitFloor = 0.55

// ok=false = 分割ではない(通常のセッション、または本物の暴落)。
func SplitRatio(prev, cur float64) (float64, bool) {
	if prev <= 0 || cur <= 0 {
		return 0, false
	}
	if math.Abs(math.Log(cur/prev)) <= SplitLogMoveThreshold {
		return 0, false
	}
	return nearestSimpleRatio(cur/prev, false)
}

// nearestSimpleRatio は値段の比 raw(cur/prev)に最も近い単純分割比を返す。
// allowShallowDown=false では下落方向の浅い比(1/1.5 等)を候補にしない — 値段だけでは
// 実暴落と区別できないから。区別できる別の証拠(値幅制限の外)を持つ呼び手だけが true にする。
func nearestSimpleRatio(raw float64, allowShallowDown bool) (float64, bool) {
	// 🛑 **最も近い候補**を選ぶ(最初に当たったものではない)。深い比は許容幅が
	// 広い(12%)ので候補どうしが重なり、リスト順に返すと隣の比を掴む
	// (7013 の 1/6.67 が 1/7 ではなく 1/6 と判定された)。
	best, bestErr, found := 0.0, math.Inf(1), false
	for _, s := range simpleSplitRatios {
		for _, cand := range [2]float64{s, 1 / s} {
			if !allowShallowDown && cand < 1 && cand > downSplitFloor {
				continue // 下落方向の浅い比(1/1.5 等)は実暴落と区別できない
			}
			tol := splitRatioTolerance
			if cand <= 0.5 || cand >= 2 {
				tol = splitRatioToleranceDeep
			}
			e := math.Abs(raw/cand - 1)
			if e <= tol && e < bestErr {
				best, bestErr, found = cand, e, true
			}
		}
	}
	return best, found
}

// 単一ソース(立花・未調整)では拒否ではなく chain-link で吸収するしかない。
func ChainLinkSplits(bars []Candle) ([]Candle, int) {
	out := append([]Candle(nil), bars...)
	adjusted := 0
	for i := len(out) - 1; i >= 1; i-- {
		ratio, ok := SplitRatio(out[i-1].Close, out[i].Close)
		if !ok {
			continue
		}
		for j := 0; j < i; j++ {
			out[j].Open *= ratio
			out[j].High *= ratio
			out[j].Low *= ratio
			out[j].Close *= ratio
			if ratio != 0 {
				out[j].Volume /= ratio
			}
		}
		adjusted++
	}
	return out, adjusted
}

// 同日水準チェックに参加する直近保存バー数。取り込みは append-only なので検証が要るのは継ぎ目付近で
// 両者の調整規約が一致しているかだけ。取得側の 250本窓に含まれる「過去の分割前の日」まで比べると、
// chain-link 済み履歴を持つ銘柄が恒久的に更新できなくなる(実際に 18 銘柄がロックされた)。
const SeamOverlapDays = 5

func RecentWindow(bars []Candle, n int) []Candle {
	if len(bars) <= n {
		return bars
	}
	return bars[len(bars)-n:]
}

// バー時刻を東京の暦日へ正規化する。pg は TIMESTAMPTZ をセッション zone(既定 UTC)で返すため、
// 正規化しないと同日チェックが黙って無効化される。
func JSTDayKey(t time.Time) string { return t.In(clock.JST).Format("2006-01-02") }
