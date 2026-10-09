package strategy

import (
	"fmt"
	"math"
	"sort"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/ta"
)

// Screener は Evaluate の入口ゲートを純粋にミラーする(I/O 無し = バックテストにも全ユニバース走査にも使える)。
// Score は「gate 超過度」: 1.0 = トリガー丁度、>1 が強い。多条件ゲートでは各条件の比の MIN(最弱脚)を取るので、
// 全条件を満たしたときだけ >=1 になる。各戦略が「自分のトリガーをどれだけ超えたか」なので戦略横断で順位付けできる。

type Condition struct {
	Label string `json:"label"` // e.g. "25日線から-12%以上の急落"
	Met   bool   `json:"met"`
	Got   string `json:"got"` // the current value, e.g. "-8.5%"
}

type Candidate struct {
	Symbol    string              `json:"symbol"`
	Strategy  config.StrategyName `json:"strategy"`
	Triggered bool                `json:"triggered"` // the strategy's ENTRY gate is satisfied on the latest bar
	Score     float64             `json:"score"`     // gate-exceedance (1.0 = at trigger); higher = stronger
	Detail    string              `json:"detail"`    // human-readable metrics
	// Side は入口が成立した**向き**(空 = 未成立、または買い専用スクリーナー)。
	//
	// 🚨 これが無いと「貸借銘柄でない銘柄の売り候補」を枠の配分前に落とせない。
	// 発注前ゲート(risk.EvaluateShortLoanable)は arm の**後**なので、建玉に
	// ならない売り候補が per_strategy_n の枠を日中ずっと占有し、同じ戦略の下位に
	// いる**買い候補が arm されなくなる** — 事前登録が宣言する「買いには一切効かない」
	// が実装で破れる形。
	Side order.Side `json:"side,omitempty"`
	// StopLossJPY は入口が成立したときの**計画ストップ幅**(円/株)。
	//
	// 🚨 これが無いと「1 本の計画損失が上限を超える候補」を枠の配分前に落とせない。
	// 発注前ゲート(`risk` の `risk_per_trade`)は arm の**後**なので、建玉にならない
	// 候補が口座の建玉枠を占有し続ける — selector は毎 Tick 同じ日足から決定論的に
	// 同じ首位を選ぶので枠が永久に空かず、「発注待ちのまま動かない沈黙のデッドロック」
	// になる(分割未調整の銘柄が起こすのと同じ形)。`Side` を足した
	// 理由(貸借でない売り候補が枠を食う)と構造は完全に同じ。
	//
	// 🛑 **0 は「算出できなかった」であって「損失ゼロ」ではない。** 出口を config から
	// 読む戦略や履歴不足では 0 のまま。読み手は 0 を上限判定に使わず素通しさせ、
	// 発注前ゲートに委ねること(ここで fail-close にすると、報告しない戦略が黙って
	// 永久に arm されなくなる)。
	StopLossJPY float64     `json:"stop_loss_jpy,omitempty"`
	Conditions  []Condition `json:"conditions"`
}

type Screener interface {
	Name() config.StrategyName
	Screen(symbol string, daily []market.Candle) Candidate
}

// 強い順(triggered → score → symbol → strategy)。tiebreak が決定的なので map 反復順に依存しない。
func RankCandidates(universe map[string][]market.Candle, screeners []Screener) []Candidate {
	out := make([]Candidate, 0, len(universe)*len(screeners))
	for sym, d := range universe {
		for _, s := range screeners {
			out = append(out, s.Screen(sym, d))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Triggered != b.Triggered {
			return a.Triggered
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		return a.Strategy < b.Strategy
	})
	return out
}

func min3(a, b, c float64) float64 { return math.Min(a, math.Min(b, c)) }

// mirrorScore は「gate 超過度」を **側に応じて**返す。
//
// 🚨 なぜ要るか: score は per_strategy_n の枠を配る順位そのもの。
// 買い専用の式(`last/sma` 等)を売り局面に当てると必ず 1 未満になるので、`Triggered` を
// 立てても **枠は常に買い候補に奪われ、売り標本は事実上ゼロのまま**になる。
//
// 🛑 **買い側の値は 1 ビットも変えない**(`long` をそのまま返す)。過去の
// score 分位との比較可能性がそこに乗っている。売り側は**同じ式の分子と分母を入れ替えた
// 鏡像**で、新しい定数は 1 つも増えない(「新しい自由パラメータが増えない」を守る)。
//
// side=="" (未成立)のときは買い側の式を返す — 従来の「発火していない候補の score」の
// 見え方を変えないため(Triggered=false なので枠には 影響 しない)。
func mirrorScore(side order.Side, long, short float64) float64 {
	if side == order.SideSell {
		return short
	}
	return long
}

// sideTag は Detail の先頭に付ける向きの目印。画面で「買いのつもりで見ていたら売りだった」を
// 起こさないため(空 = 買い or 未成立で、従来の表示と同じ)。
func sideTag(side order.Side) string {
	if side == order.SideSell {
		return "[売] "
	}
	return ""
}

// --- 各 Screen は対応する Evaluate の入口ゲートを厳密にミラーする ---

// 既定閾値で screen する(config の tuning override は見ない)。ランキング精度のみの割り切りで、
// 建玉時の権威は risk gate + Evaluate。
func (BNFReversion) Screen(symbol string, d []market.Candle) Candidate {
	c := Candidate{Symbol: symbol, Strategy: config.StrategyBNFReversion}
	if len(d) < bnfSMA+1 {
		c.Detail = "insufficient_history"
		return c
	}
	sma := ta.SMA(closesOf(d), bnfSMA)
	if sma <= 0 {
		c.Detail = "no_sma"
		return c
	}
	volAvg := avgVolume(d, bnfVolSMA)
	if volAvg <= 0 {
		c.Detail = "no_volume"
		return c
	}
	last := d[len(d)-1]
	dev := (last.Close - sma) / sma
	volRatio := last.Volume / volAvg
	c.Score = math.Min(dev/bnfDevThreshold, volRatio/bnfVolRatio) // bnfDevThreshold<0 → dev/thr>0
	c.Triggered = dev <= bnfDevThreshold && volRatio >= bnfVolRatio
	c.Detail = fmt.Sprintf("dev %+.1f%% / vol %.2fx", dev*100, volRatio)
	c.Conditions = []Condition{
		{Label: "25日線から-12%以上の急落", Met: dev <= bnfDevThreshold, Got: fmt.Sprintf("%+.1f%%", dev*100)},
		{Label: "出来高1.5倍以上(パニック)", Met: volRatio >= bnfVolRatio, Got: fmt.Sprintf("%.2fx", volRatio)},
	}
	// 計画ストップ幅(枠を配る前の重さ判定用)。**Evaluate と同じ幾何を同じ関数から**
	// 引く — 片方だけ動かすと、arm では通したのに発注前ゲートが必ず落とす銘柄が
	// 静かに生まれる。ATR が取れない系列では 0 のまま(= 判定材料なし)。
	if atr := ta.ATR(d, bnfATRPeriod); atr > 0 {
		_, c.StopLossJPY = bnfReversionExitJPY(last.Close, sma, bnfStopATR, atr)
	}
	return c
}

// 入口は BNFReversion と同一(差は出口だけ)。スクリーナーが無いとランキングに現れず advisor の候補にも
// ならないため標本が永久にゼロになる(実測: trail は1件も選ばれていなかった)。
func (BNFReversionTrail) Screen(symbol string, d []market.Candle) Candidate {
	c := BNFReversion{}.Screen(symbol, d)
	c.Strategy = config.StrategyBNFReversionTrail
	if c.Detail != "" && c.Triggered {
		c.Detail += " / 出口=ATRトレール(利伸ばし)"
	}
	return c
}

func (HighVolumePremium) Screen(symbol string, d []market.Candle) Candidate {
	c := Candidate{Symbol: symbol, Strategy: config.StrategyHighVolumePremium}
	if len(d) < hvpTrendSMA+1 {
		c.Detail = "insufficient_history"
		return c
	}
	last := d[len(d)-1]
	prior := d[len(d)-hvpLookback : len(d)-1]
	maxVol, sumVol := 0.0, 0.0
	for _, x := range prior {
		if x.Volume > maxVol {
			maxVol = x.Volume
		}
		sumVol += x.Volume
	}
	avg := sumVol / float64(len(prior))
	if avg <= 0 || maxVol <= 0 {
		c.Detail = "no_volume"
		return c
	}
	sma100 := ta.SMA(closesOf(d), hvpTrendSMA)
	if sma100 <= 0 {
		c.Detail = "no_sma"
		return c
	}
	c.Score = min3(last.Volume/maxVol, (last.Volume/avg)/hvpMinMult, last.Close/sma100)
	c.Triggered = last.Volume > maxVol && last.Volume >= hvpMinMult*avg && last.Close >= sma100
	c.Detail = fmt.Sprintf("vol %.2fx avg / %.0f%% of 50d-max / close %+.1f%% vs 100MA",
		last.Volume/avg, last.Volume/maxVol*100, (last.Close/sma100-1)*100)
	c.Conditions = []Condition{
		{Label: "出来高が過去50日で最大", Met: last.Volume > maxVol, Got: fmt.Sprintf("%.0f%% of max", last.Volume/maxVol*100)},
		{Label: "出来高2倍以上", Met: last.Volume >= hvpMinMult*avg, Got: fmt.Sprintf("%.2fx", last.Volume/avg)},
		{Label: "100日線より上(トレンド維持)", Met: last.Close >= sma100, Got: fmt.Sprintf("%+.1f%%", (last.Close/sma100-1)*100)},
	}
	return c
}

func (PostJumpDrift) Screen(symbol string, d []market.Candle) Candidate {
	c := Candidate{Symbol: symbol, Strategy: config.StrategyPostJumpDrift}
	if len(d) < peadVolWindow+2 {
		c.Detail = "insufficient_history"
		return c
	}
	prev, last := d[len(d)-2].Close, d[len(d)-1].Close
	if prev <= 0 {
		c.Detail = "bad_prev_close"
		return c
	}
	ret := last/prev - 1
	rs := make([]float64, 0, peadVolWindow)
	for i := len(d) - peadVolWindow; i < len(d); i++ {
		if p0 := d[i-1].Close; p0 > 0 {
			rs = append(rs, d[i].Close/p0-1)
		}
	}
	if len(rs) < 20 {
		c.Detail = "insufficient_returns"
		return c
	}
	sigma := stdev(rs)
	if sigma <= 0 {
		c.Detail = "no_vol"
		return c
	}
	volAvg := avgVolume(d, 25)
	if volAvg <= 0 {
		c.Detail = "no_volume"
		return c
	}
	volRatio := d[len(d)-1].Volume / volAvg
	c.Score = math.Min(ret/(peadJumpSigma*sigma), volRatio/peadVolMult)
	c.Triggered = ret >= peadJumpSigma*sigma && volRatio >= peadVolMult
	c.Detail = fmt.Sprintf("ret %+.1f%% (%.1fσ) / vol %.2fx", ret*100, ret/sigma, volRatio)
	c.Conditions = []Condition{
		{Label: "+2.5σ以上の上方ジャンプ", Met: ret >= peadJumpSigma*sigma, Got: fmt.Sprintf("%.1fσ", ret/sigma)},
		{Label: "出来高3倍以上", Met: volRatio >= peadVolMult, Got: fmt.Sprintf("%.2fx", volRatio)},
	}
	return c
}

// 母集団標準偏差(PEAD の Evaluate と同一式)。
func stdev(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mu := sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		ss += (x - mu) * (x - mu)
	}
	return math.Sqrt(ss / float64(len(xs)))
}

func (High52wMomentum) Screen(symbol string, d []market.Candle) Candidate {
	c := Candidate{Symbol: symbol, Strategy: config.StrategyHigh52wMomentum}
	if len(d) < h52Window+1 {
		c.Detail = "insufficient_history"
		return c
	}
	cl := closesOf(d)
	hi, lo, ok := ta.Donchian(d[:len(d)-1], h52Window)
	sma := ta.SMA(cl, absTrendSMA)
	if !ok || hi <= 0 || lo <= 0 || sma <= 0 {
		c.Detail = "no_high_or_sma"
		return c
	}
	last := d[len(d)-1].Close
	// 52週**安値**ブレイクの売りも入口の式に元からある。
	side, _ := high52wSideAt(d)
	c.Triggered = side != ""
	c.Side = side
	c.Score = mirrorScore(side, math.Min(last/(hi*0.99), last/sma), math.Min((lo*1.01)/last, sma/last))
	if side == order.SideSell {
		c.Detail = fmt.Sprintf("[売] %.1f%% of 52w-low / close %+.1f%% vs 200MA", last/lo*100, (last/sma-1)*100)
		c.Conditions = []Condition{
			{Label: "52週安値の1%以内(安値圏)", Met: last <= lo*1.01, Got: fmt.Sprintf("%.1f%% of low", last/lo*100)},
			{Label: "200日線より下(トレンド)", Met: last < sma, Got: fmt.Sprintf("%+.1f%%", (last/sma-1)*100)},
		}
		return c
	}
	c.Detail = fmt.Sprintf("%.1f%% of 52w-high / close %+.1f%% vs 200MA", last/hi*100, (last/sma-1)*100)
	c.Conditions = []Condition{
		{Label: "52週高値の1%以内(高値圏)", Met: last >= hi*0.99, Got: fmt.Sprintf("%.1f%% of high", last/hi*100)},
		{Label: "200日線より上(トレンド)", Met: last > sma, Got: fmt.Sprintf("%+.1f%%", (last/sma-1)*100)},
	}
	return c
}

func (DonchianBreakout) Screen(symbol string, d []market.Candle) Candidate {
	c := Candidate{Symbol: symbol, Strategy: config.StrategyDonchianBreakout}
	if len(d) < dbWindow+1 {
		c.Detail = "insufficient_history"
		return c
	}
	hi, lo, ok := ta.Donchian(d[:len(d)-1], dbWindow)
	if !ok || hi <= 0 || lo <= 0 {
		c.Detail = "no_channel"
		return c
	}
	last := d[len(d)-1].Close
	// 下方ブレイク(20日安値割れ)も入口の式に元からある。screener 側だけが
	// 見ていなかった。
	side := donchianSideAt(d)
	c.Triggered = side != ""
	c.Side = side
	c.Score = mirrorScore(side, last/hi, lo/last)
	if side == order.SideSell {
		c.Detail = fmt.Sprintf("[売] %.1f%% of 20d-low", last/lo*100)
		c.Conditions = []Condition{
			{Label: "20日安値ブレイク", Met: true, Got: fmt.Sprintf("%.1f%% of low", last/lo*100)},
		}
		return c
	}
	c.Detail = fmt.Sprintf("%.1f%% of 20d-high", last/hi*100)
	c.Conditions = []Condition{
		{Label: "20日高値ブレイク", Met: c.Triggered, Got: fmt.Sprintf("%.1f%% of high", last/hi*100)},
	}
	return c
}

func (ATRBreakout) Screen(symbol string, d []market.Candle) Candidate {
	c := Candidate{Symbol: symbol, Strategy: config.StrategyATRBreakout}
	if len(d) < atrTrendSMA+1 {
		c.Detail = "insufficient_history"
		return c
	}
	atr := ta.ATR(d, atrN)
	sma := ta.SMA(closesOf(d), atrTrendSMA)
	if atr <= 0 || sma <= 0 {
		c.Detail = "no_atr_or_sma"
		return c
	}
	prevClose := d[len(d)-2].Close
	last := d[len(d)-1].Close
	// 下方への拡大も入口の式に元からある(v1 相当のヘルパを共有する。
	// 圧縮条件は v2 の追加なので、ここでは見ない — ATRBreakoutV2.Screen が足す)。
	side, _ := atrRangeExpansionSideAt(d)
	c.Triggered = side != ""
	c.Side = side
	c.Score = mirrorScore(side, math.Min((last-prevClose)/atr, last/sma), math.Min((prevClose-last)/atr, sma/last))
	c.Detail = fmt.Sprintf("%smove %+.2f ATR / close %+.1f%% vs 100MA",
		sideTag(side), (last-prevClose)/atr, (last/sma-1)*100)
	if side == order.SideSell {
		c.Conditions = []Condition{
			{Label: "-1ATR 超のレンジ拡大", Met: last < prevClose-atr, Got: fmt.Sprintf("%+.2f ATR", (last-prevClose)/atr)},
			{Label: "100日線より下(トレンド)", Met: last < sma, Got: fmt.Sprintf("%+.1f%%", (last/sma-1)*100)},
		}
		return c
	}
	c.Conditions = []Condition{
		{Label: "+1ATR 超のレンジ拡大", Met: last > prevClose+atr, Got: fmt.Sprintf("%+.2f ATR", (last-prevClose)/atr)},
		{Label: "100日線より上(トレンド)", Met: last > sma, Got: fmt.Sprintf("%+.1f%%", (last/sma-1)*100)},
	}
	return c
}

func (AbsMomentum) Screen(symbol string, d []market.Candle) Candidate {
	c := Candidate{Symbol: symbol, Strategy: config.StrategyAbsMomentum}
	if len(d) < absTrendSMA+1 {
		c.Detail = "insufficient_history"
		return c
	}
	cl := closesOf(d)
	sma := ta.SMA(cl, absTrendSMA)
	if sma <= 0 {
		c.Detail = "no_sma"
		return c
	}
	past := cl[len(cl)-1-absLookback]
	if past <= 0 {
		c.Detail = "no_past_close"
		return c
	}
	last := d[len(d)-1].Close
	// 🚨 **入口と同じヘルパで side を判定する**。
	// ここが `last > sma && last > past` の買い専用だったため、売りの入口が成立している
	// 銘柄は一度も Triggered にならず、枠が配られず、`Evaluate` が売りを計算する機会が
	// 来なかった = **事前登録は direction を開けても構造的に標本ゼロ**だった。
	side := absMomentumSideAt(d)
	c.Triggered = side != ""
	c.Side = side
	c.Score = mirrorScore(side, math.Min(last/sma, last/past), math.Min(sma/last, past/last))
	c.Detail = fmt.Sprintf("%sclose %+.1f%% vs 200MA / %+.1f%% vs 6mo-ago",
		sideTag(side), (last/sma-1)*100, (last/past-1)*100)
	if side == order.SideSell {
		c.Conditions = []Condition{
			{Label: "200日線より下", Met: last < sma, Got: fmt.Sprintf("%+.1f%%", (last/sma-1)*100)},
			{Label: "6ヶ月前の水準より下", Met: last < past, Got: fmt.Sprintf("%+.1f%%", (last/past-1)*100)},
		}
		return c
	}
	c.Conditions = []Condition{
		{Label: "200日線より上", Met: last > sma, Got: fmt.Sprintf("%+.1f%%", (last/sma-1)*100)},
		{Label: "6ヶ月前の水準より上", Met: last > past, Got: fmt.Sprintf("%+.1f%%", (last/past-1)*100)},
	}
	return c
}
