package risk

import (
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/strategy"
)

// 値幅制限(ストップ高/安)のゲート。**live で踏んだ事故の再発防止。**
//
// 事故の形: 25日線 6,435 の TP で建てようとしたが、当日の値幅制限上限は 6,352
// (基準値段 5,352 ± 1,000)。約定の**後**に守りの発注が立花に拒否され
// ("当該銘柄の値幅制限内の単価を入力してください")、約定済みの建玉を閉じる補償が走った。
// 建玉は台帳に書かれないまま閉じるので、次のティックで bot は「持っていない」と判断して
// また建てた — 同じ銘柄で 5 往復。市場は正しく動いており、bot だけが値幅制限を知らなかった。
//
// 🛑 **TP と SL で扱いを変える**(CLAUDE.md「SL 欠落は致命的、TP 欠落は機会損失のみ」)。
//   - SL が帯の外 → **建てない**。守りを board に置けない建玉は作らない。
//   - TP が帯の外 → **建てる**。TP 脚だけ落として OnTick で見る(ProtectiveTakeProfitPlaceable)。
//     ここで entry ごと止めると、「急落が深いほど 25日線が遠い」BNF の**一番強いシグナルだけ**が
//     系統的に消え、forward 標本が歪む(消えたことすら台帳に残らない)。
//
// 基準値段は前日終値(権利落ち日は最終気配値段)。運用側の日足は fetch-daily が
// ChainLinkSplits を通してから保存しているので、呼び手はその終値をそのまま渡してよい。
func EvaluatePriceLimit(sig strategy.Signal, refPrice float64) Decision {
	if sig.StopLossJPY <= 0 {
		return Decision{Allowed: true} // 守り幅ゼロは別のゲートの担当
	}
	up, down, ok := priceLimitBand(refPrice)
	if !ok {
		// 確かめられないことを大丈夫と読まない。基準値段が無い = 日足が無い/古い ので、
		// そもそも日足戦略の entry を通してよい状態ではない。
		return Decision{Allowed: false, Reason: "price_limit_reference_unavailable"}
	}
	_, sl := position.TPSLPricesFromJPY(sig.Symbol, sig.Side, sig.EntryPrice, 0, sig.StopLossJPY)
	if sl <= 0 {
		return Decision{Allowed: false, Reason: "price_limit_reference_unavailable"}
	}
	if sl < down || sl > up {
		return Decision{Allowed: false, Reason: "stop_loss_outside_price_limit"}
	}
	return Decision{Allowed: true}
}

// ProtectiveTakeProfitPlaceable は「利確の指値を今日の板に置けるか」。置けないときは
// TP 脚を落として SL のみ broker 側に置く(立花の PlaceSettleOCO は TakeProfit=0 で
// 逆指値のみの形を受ける)。**TP は OnTick が引き継ぐ**ので守りは落ちない。
func ProtectiveTakeProfitPlaceable(side order.Side, tpPrice, refPrice float64) bool {
	if tpPrice <= 0 {
		return false // そもそも TP 脚が無い
	}
	_ = side // 帯の内側かどうかは向きに依らない。side は呼び手の可読性のために受ける
	in, ok := PriceInsideLimitBand(tpPrice, refPrice)
	if !ok {
		return false // 確かめられないなら置きに行かない(拒否されると建玉ごと巻き戻る)
	}
	return in
}

// ProtectiveTakeProfitOnBoard は「守りの注文に TP 脚を載せるか」。全経路(新規建て・
// 消えた守りの復旧・期日の置き直し・値段変更)がこれ 1 つで決める。
//
// 🛑 **多日建玉は載せない(stop-only)。** 立花は期日付きの
// 返済注文を毎営業日に翌日へ繰り越すとき翌日の値幅制限で再検査し、**TP 脚が帯の外に
// 出ていると SL 脚ごと失効させる**(例: ある朝 TP 3,305 > 帯上限 3,184 と
// TP 5,366 > 5,218 の 2 建玉の守りが消え、TP の無い建玉と TP が帯内の建玉は残った。
// 別の日にも同型)。急落した翌朝 = 損切りが一番要る局面ほど SL が消える構造なので、多日の
// TP は台帳(take_profit_price)に残して OnTick が持つ(CLAUDE.md「SL 欠落は致命的、
// TP 欠落は機会損失のみ」)。holding が空(不明)も多日側に倒す(fail-safe は SL が残る側)。
//
// intraday は当日限りで繰越が無いので従来どおり: 帯の内側なら載せ、外なら落とす。基準値段が
// 無いときは載せる(判定できないことを理由に守りの形を変えない)。
//
// 戻り値の reason は呼び手がログに残すためのもの(GROUP BY キーにはしない):
// no_take_profit / multiday_stop_only / reference_unavailable / outside_price_limit / inside_price_limit。
func ProtectiveTakeProfitOnBoard(holding order.HoldingMode, side order.Side, tpPrice, refPrice float64) (place bool, reason string) {
	if tpPrice <= 0 {
		return false, "no_take_profit"
	}
	if holding != order.HoldingIntraday {
		return false, "multiday_stop_only"
	}
	if refPrice <= 0 {
		return true, "reference_unavailable"
	}
	if !ProtectiveTakeProfitPlaceable(side, tpPrice, refPrice) {
		return false, "outside_price_limit"
	}
	return true, "inside_price_limit"
}

// PriceInsideLimitBand は「その値段が今日の値幅制限の内側か」。ok=false は
// **判定できなかった**(基準値段が無い / 表の外)であって「外だった」ではない ——
// 呼び手はこの 2 つを混ぜないこと。
//
// 🚨 **脚が 1 本でも帯の外だと立花は注文ごと拒否する**(7220 で実測。
// TP だけが 42 円はみ出しただけで、SL も一緒に置けず建玉は裸のまま残った)。
// だから「TP は落とせる / SL は落とせない」の判定に、脚ごとにこれを使う。
func PriceInsideLimitBand(price, refPrice float64) (inside, ok bool) {
	if price <= 0 {
		return false, false
	}
	up, down, ok := priceLimitBand(refPrice)
	if !ok {
		return false, false
	}
	return price >= down && price <= up, true
}

// LimitBandFor は帯そのものを返す。**人間に返すエラー文へ数字を載せる**ためにある ——
// 立花の拒否は `当該銘柄の値幅制限内の単価を入力してください` としか言わず、
// **どちらの脚が外なのか・帯がいくつなのかが分からない**(実際に 2 回踏んだ)。
func LimitBandFor(refPrice float64) (up, down float64, ok bool) { return priceLimitBand(refPrice) }

func priceLimitBand(refPrice float64) (up, down float64, ok bool) {
	if refPrice <= 0 {
		return 0, 0, false
	}
	u, okU := market.LimitUp(refPrice)
	d, okD := market.LimitDown(refPrice)
	if !okU || !okD {
		return 0, 0, false
	}
	return u, d, true
}
