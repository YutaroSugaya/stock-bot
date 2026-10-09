package command

import (
	"context"
	"fmt"
	"sort"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
)

// replaceProtectiveWithin は「残り営業日がこれ以下になったら置き直す」閾値。
//
// 🛑 **なぜ閾値が要るのか。**置き直しは取消と再発注の間に**守りが完全に消える窓**を
// 開ける。毎朝すべての守りを無条件に置き直すと、その窓を毎日・全建玉ぶん開けること
// になる。窓を開ける回数は「期日が切れる直前の 1 回」で足りる。
//
// 2 なのは、置いた守りが `settleMaxTradingDays`(9営業日)先まで生きるので、1 営業日
// 動かなくても切れない余裕を残しつつ、無駄撃ちしないため。朝の窓は 1 日 1 回しか
// 来ないので、閾値 1 だと bot が 1 日止まっただけで裸になる。
const replaceProtectiveWithin = 2

// ReplaceProtectiveResult は 1 周ぶんの結果。
//
// 🛑 **Skipped を返すのは `Replaced: 0` の意味を割るため。**「期日が近いものが無くて
// 何もしなかった」と「対象はあったが全部失敗した」は運用上まったく別の状態で、
// 前者を後者と読むと毎朝の正常運転が異常に見え、後者を前者と読むと裸を見逃す。
type ReplaceProtectiveResult struct {
	Replaced int // 取消 → 再発注できた本数
	Skipped  int // まだ期日に余裕があって触らなかった本数
}

// ReplaceProtectiveOrder は **取消 → 同条件で再発注**して守りの期日を延ばす。
//
// 🚨 なぜ訂正ではないのか:
// 立花の `sOrderExpireDay`「10営業日迄」は**発注日起点**で、訂正しても天井は動かない。
// 一次資料の CLMOrderListDetail が当日限りの注文にも `sOrderExpireDayLimit` =
// 発注日+10営業日 を返しているのが決め手。天井より先の期日を投げても
// 拒否されるだけで、アプリの訂正画面でも期日はほとんど選べない。
// **期日を延ばす手段は出し直ししかない。**
//
// 🛑 **cancel → place の間は守りが完全に消える。** CLAUDE.md「TP/SL は必ず broker 側」
// に対する意図的な、時間の限られた例外。だから 3 つの縛りを置く:
//
//  1. **場外でしか撃たない**(下の InTradingHours ガード)。値が動かない時間帯なら
//     窓が開いていても価格リスクは無い。
//  2. **取消が失敗したら再発注しない**。守りは板に残っているので現状維持が正しい
//     (ここで発注すると守りが二重になり、返済可能数量超過で両方弾かれうる)。
//  3. **再発注が失敗したら emergency を trip する**。取消は通ったのに置き直せない =
//     建玉が裸のまま。新規を止めて叫ぶ以外にできることは無い。
//
// 🛑 **値段は板から引き継ぐ。Position の凍結値を使わない。** 人間が手で TP/SL を
// 締めていることがある。凍結値で出し直すと
// その調整を勝手に元の広い幅へ戻してしまう。
type ReplaceProtectiveOrder struct {
	positions port.PositionRepository
	broker    protectiveReplacer
	hours     session.TradingHours
	clock     clock.Clock
	emergency EmergencyController
	log       func(msg string, kv ...any)
	// refPrice は値幅制限の基準値段(前日終値)。nil / 0 は「判定できない」= 帯は見ない。
	refPrice func(ctx context.Context, symbol string) float64
}

// protectiveReplacer は取消と再発注に要る面だけ。
type protectiveReplacer interface {
	port.ProtectiveOrderBoard
	PlaceSettleOCO(ctx context.Context, in port.OCOCloseOrderInput) (string, error)
}

func NewReplaceProtectiveOrder(pr port.PositionRepository, brk protectiveReplacer,
	hours session.TradingHours, clk clock.Clock, em EmergencyController) *ReplaceProtectiveOrder {
	return &ReplaceProtectiveOrder{positions: pr, broker: brk, hours: hours, clock: clk, emergency: em}
}

func (r *ReplaceProtectiveOrder) WithLogger(fn func(msg string, kv ...any)) *ReplaceProtectiveOrder {
	r.log = fn
	return r
}

// WithPriceLimitRef は値幅制限の基準値段(前日終値)の引き口を挿す(ArmProtectiveOrder と同じ)。
func (r *ReplaceProtectiveOrder) WithPriceLimitRef(fn func(ctx context.Context, symbol string) float64) *ReplaceProtectiveOrder {
	r.refPrice = fn
	return r
}

func (r *ReplaceProtectiveOrder) logf(msg string, kv ...any) {
	if r.log != nil {
		r.log(msg, kv...)
	}
}

// Execute は 1 周ぶん。
func (r *ReplaceProtectiveOrder) Execute(ctx context.Context) (ReplaceProtectiveResult, []error) {
	var res ReplaceProtectiveResult
	now := r.clock()

	// 🛑 **場中は撃たない。** 取消と再発注の間に守りが消える窓が開くので、
	// 値が動く時間帯には入らない。ここは呼び出し側と二重にガードする —
	// 片方だけだと配線を変えたときに黙って場中に撃つ形になりうる。
	if r.hours.InTradingHours(now) {
		return res, nil
	}

	guarded, err := multidayGuardedPositions(ctx, r.positions)
	if err != nil {
		return res, []error{fmt.Errorf("守りの置き直し: 建玉を読めない: %w", err)}
	}
	syms := make([]string, 0, len(guarded))
	for sym := range guarded {
		syms = append(syms, sym)
	}
	sort.Strings(syms)

	newExpiry := r.hours.NthTradingDayFrom(now, settleMaxTradingDays)
	if newExpiry.IsZero() {
		return res, []error{fmt.Errorf("🚨 守りの置き直し: 休場カレンダーが尽きていて新しい期日を出せない — " +
			"**取り消してはいけない**(置き直せないまま裸になる)。hard_limits の holidays / calendar_through を更新すること")}
	}
	// 🛑 閾値の判定日。カレンダー切れなら zero になるが、その場合は上で既に抜けている。
	deadline := r.hours.NthTradingDayFrom(now, replaceProtectiveWithin)

	var errs []error
	for _, sym := range syms {
		orders, err := r.broker.ListProtectiveOrders(ctx, sym)
		if err != nil {
			errs = append(errs, fmt.Errorf("守りの置き直し %s: 注文照会に失敗: %w", sym, err))
			continue
		}
		for _, o := range orders {
			if !o.HasStopLeg || !ownsProtectiveOrder(guarded[sym], o) {
				continue
			}
			// 既に新しい期日以上なら触らない(出し直す意味が無い)。
			if !o.ExpireOn.IsZero() && !newExpiry.After(o.ExpireOn) {
				res.Skipped++
				continue
			}
			// 🛑 **期日が近いものだけ。**needsRenewal と同じ判定を使う —— zero
			// (当日限り / 期日が読めない)を「余裕がある」と読まないのが肝。
			if !needsRenewal(o.ExpireOn, deadline) {
				res.Skipped++
				continue
			}
			n, err := r.replaceOne(ctx, sym, guarded[sym], o, newExpiry, now)
			res.Replaced += n
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	return res, errs
}

// replaceOne は 1 本ぶん。**取消 → 再発注**の順で、途中で失敗したら適切に止める。
func (r *ReplaceProtectiveOrder) replaceOne(ctx context.Context, sym string,
	held []position.Position, o port.ProtectiveOrderInfo, newExpiry, now time.Time) (int, error) {

	// 🛑 値段が読めないまま取り消さない。取消は通って再発注が組めない = 裸になる。
	if o.StopTrigger <= 0 {
		return 0, fmt.Errorf("🚨 守りの置き直し %s(注文 %s): 逆指値のトリガー価格を照会から読めない — "+
			"**取り消さない**(置き直せないまま裸になる)。板の注文はそのまま残す", sym, o.OrderID)
	}

	// 🚨 **どの建玉の守りかを取消の前に確定させる**。
	// 再発注には板から読めない項目 —— 口座区分(現物/制度信用)と建玉 ID —— が要る。
	// これを**台帳から**取らずにゼロ値のまま送ったため、制度信用の建玉に現物の返済を
	// 撃って拒否され、取消済みの 3 建玉が寄り前に裸で残った。
	// 特定できないなら**取り消さない**。板に守りが残っているほうが常にましで、
	// 「取消してから考える」は守りの経路では常に間違い。
	pos, ok := matchProtectivePosition(held, o)
	if !ok {
		return 0, fmt.Errorf("🚨 守りの置き直し %s(注文 %s・%d株): この注文がどの建玉の守りか台帳から確定できない — "+
			"**取り消さない**(口座区分と建玉 ID が分からないと再発注が組めず裸になる)。板の注文はそのまま残す",
			sym, o.OrderID, o.Quantity)
	}

	// 🚨 **今日の値幅制限で脚を検問してから取り消す**。前日終値が
	// 動くと帯がずれ、板から引き継ぐ値段が外に出る(7220: TP が 42 円はみ出た)。
	// 立花は脚が 1 本でも帯の外だと注文ごと拒否するので、取消の後に気づくと裸 + trip。
	// SL が外なら**取り消さない**(板の守りは期日までまだ守り)。TP は落とすだけ。
	tp := o.LimitPrice
	replaceRef := 0.0
	if r.refPrice != nil {
		if ref := r.refPrice(ctx, sym); ref > 0 {
			replaceRef = ref
			if inside, ok := risk.PriceInsideLimitBand(o.StopTrigger, ref); ok && !inside {
				up, down, _ := risk.LimitBandFor(ref)
				return 0, fmt.Errorf("🚨 守りの置き直し %s(注文 %s): 逆指値(SL) %g が今日の値幅制限の外 "+
					"(基準値段 %g / 帯 %g〜%g)— **取り消さない**(置き直せず裸になる)。"+
					"帯の内側の SL で守りの値段変更(reprice)を使うこと",
					sym, o.OrderID, o.StopTrigger, ref, down, up)
			}
		}
	}
	// 多日建玉は帯の内外に依らず **stop-only**(risk.ProtectiveTakeProfitOnBoard)。
	// 板に TP 脚が載っていても(人間の手置き・旧仕様の OCO)引き継がず、SL だけ同条件で置き直す。
	if place, reason := risk.ProtectiveTakeProfitOnBoard(pos.HoldingMode, o.Side, tp, replaceRef); !place && tp > 0 {
		r.logf("守りの置き直し: TP 脚は板に載せない(SL のみ)", "symbol", sym, "order", o.OrderID, "tp", tp, "reason", reason,
			"理由", "多日の守りは stop-only(繰越時に TP が帯の外だと SL ごと失効する)。TP は OnTick が持つ")
		tp = 0
	}

	if err := r.broker.CancelProtectiveOrder(ctx, o); err != nil {
		// 🛑 取消が失敗 = 守りは板に残っている。**現状維持が正しい**ので trip しない。
		return 0, fmt.Errorf("守りの置き直し %s(注文 %s): 取消に失敗 — 板の守りはそのまま残っている(期日 %s): %w",
			sym, o.OrderID, expiryLabel(o.ExpireOn), err)
	}

	// 🛑 **出どころが 2 つある。**取り違えるとどちらの方向にも事故る:
	//   値段 → **板**(人間が手で締めた幅を凍結値で上書きしない)
	//   区分・建玉 ID → **台帳**(板の照会からは読めない。ゼロ値 = 現物になる)
	in := port.OCOCloseOrderInput{
		Symbol:           sym,
		BrokerPositionID: pos.BrokerPositionID,
		Side:             o.Side,
		Quantity:         o.Quantity,
		TakeProfit:       tp,
		StopLoss:         o.StopTrigger,
		// 🚨 これが無いとゼロ値 "" → adapter の default 節 → "0"(現物)。
		// 制度信用の建玉には「口座区分がお預かり銘柄と不一致」で拒否される。
		ExecKind: pos.ExecKind,
		ExpireOn: newExpiry,
	}
	if _, err := r.broker.PlaceSettleOCO(ctx, in); err != nil {
		// 🚨 取消は通ったのに置き直せない = **この建玉はいま裸**。
		// 新規を止めて叫ぶ以外にできることは無い。
		reason := "protective_replace_failed:" + sym
		if r.emergency != nil {
			_ = r.emergency.Trip(reason, now)
		}
		return 0, fmt.Errorf("🚨🚨 守りの置き直し %s(注文 %s): 取消は通ったが再発注に失敗 — "+
			"**この建玉はいま broker 側の守りが無い**。emergency を trip した(新規は止まる)。"+
			"復旧: `POST /api/live/protective/arm {\"symbol\":\"%s\",\"take_profit\":%.1f,\"stop_loss\":%.1f}` "+
			"か、証券アプリから同じ値段の決済注文(制度信用の返済)を置くこと: %w",
			sym, o.OrderID, sym, o.LimitPrice, o.StopTrigger, err)
	}

	r.logf("守りを置き直した(取消→再発注)", "symbol", sym, "old_order", o.OrderID,
		"was", expiryLabel(o.ExpireOn), "now", newExpiry.Format("2006-01-02"),
		"tp", o.LimitPrice, "sl", o.StopTrigger)
	return 1, nil
}

// ownsProtectiveOrder / matchProtectivePosition / multidayGuardedPositions /
// expiryLabel は renew_protective_expiry.go と共有する(同じ「bot の守りか」の
// 判定を 2 通り持たない)。
