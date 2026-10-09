package command

import (
	"context"
	"errors"
	"fmt"
	"math"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
)

// RepriceProtectiveOrder は **守りの値段を変える**(取消 → 指定した値段で再発注)。
//
// 🚨 なぜ ReplaceProtectiveOrder と別なのか:
// あちらは「板の値段を引き継いで**期日だけ**延ばす」もので、値段を変える口が無い
// (むしろ人間が手で締めた値を戻さないよう、意図的に板から引き継いでいる)。
// こちらは**人間が指定した値段**で置き直す。
//
// 🛑 **場中でも撃つ**。取消と再発注の間は
// 守りが板から消える。値が動く時間帯にその窓を開けるのは承知の上の判断。
// 代わりに **取消の前に検証を全部済ませる**:
//
//	① 建玉を台帳から 1 つに確定(口座区分・建玉 ID・数量がここから来る)
//	② SL があること
//	③ 値段の向き(買い建玉なら TP > SL)
//	④ 板に取り消す守りが実在すること
//	⑤ 呼値の格子に載っていること(adapter は丸めない = 格子外はそのまま拒否される)
//	⑥ 値幅制限の帯(SL が外なら中止、TP が外なら TP 脚だけ落とす)
//
// 「取消してから再発注が組めないと分かる」形の事故を防ぐ。
// **取り消す前に分かることは、全部取り消す前に確かめる。**
type RepriceProtectiveOrder struct {
	positions port.PositionRepository
	broker    protectiveReplacer
	hours     session.TradingHours
	clock     clock.Clock
	emergency EmergencyController
	log       func(msg string, kv ...any)
	// refPrice は値幅制限の基準値段(前日終値)。nil / 0 は「判定できない」= 帯は見ない。
	refPrice func(ctx context.Context, symbol string) float64
}

// RepriceProtectiveInput は人間が指定する部分。**値段だけ** ——
// 数量・side・口座区分・建玉 ID は台帳から取る(打ち間違いが建玉超過の返済注文になる)。
type RepriceProtectiveInput struct {
	Symbol     string
	TakeProfit float64 // 0 = 利確脚なし(SL のみの守り)
	StopLoss   float64 // 必須
}

func NewRepriceProtectiveOrder(pr port.PositionRepository, brk protectiveReplacer,
	hours session.TradingHours, clk clock.Clock, em EmergencyController) *RepriceProtectiveOrder {
	return &RepriceProtectiveOrder{positions: pr, broker: brk, hours: hours, clock: clk, emergency: em}
}

func (r *RepriceProtectiveOrder) WithLogger(fn func(msg string, kv ...any)) *RepriceProtectiveOrder {
	r.log = fn
	return r
}

// WithPriceLimitRef は値幅制限の基準値段(前日終値)の引き口を挿す(ArmProtectiveOrder と同じ)。
func (r *RepriceProtectiveOrder) WithPriceLimitRef(fn func(ctx context.Context, symbol string) float64) *RepriceProtectiveOrder {
	r.refPrice = fn
	return r
}

// Execute は 1 銘柄ぶん。返り値は (新しい注文 ID, エラー)。
func (r *RepriceProtectiveOrder) Execute(ctx context.Context, in RepriceProtectiveInput) (string, error) {
	now := r.clock()

	// ── ① 建玉を確定する(取消より前) ──────────────────────────────
	guarded, err := multidayGuardedPositions(ctx, r.positions)
	if err != nil {
		return "", fmt.Errorf("守りの値段変更 %s: 建玉を読めない — **取り消さない**: %w", in.Symbol, err)
	}
	held := guarded[in.Symbol]
	if len(held) != 1 {
		return "", fmt.Errorf("守りの値段変更 %s: 対象の多日建玉を 1 つに確定できない(%d 本)— "+
			"**取り消さない**(口座区分と建玉 ID が分からないと再発注が組めず裸になる)", in.Symbol, len(held))
	}
	pos := held[0]

	// ── ② SL があること ────────────────────────────────────────
	if in.StopLoss <= 0 {
		return "", fmt.Errorf("守りの値段変更 %s: 逆指値(SL)の価格が要る — "+
			"**取り消さない**(TP だけでは下方向が裸のまま)", in.Symbol)
	}

	// ── ③ 値段の向き ──────────────────────────────────────────
	// 🚨 買い建玉の守りは **TP が上・SL が下**。取り違えると「利確が現値より下」に
	// なり、取消済みなら建玉を意図せず投げ捨てる。人間が数字を入れる経路なので必ず落とす。
	if in.TakeProfit > 0 {
		if pos.Side == order.SideBuy && in.TakeProfit <= in.StopLoss {
			return "", fmt.Errorf("守りの値段変更 %s: 買い建玉なのに TP(%.1f)が SL(%.1f)以下 — "+
				"**取り消さない**(TP と SL が入れ替わっていないか確認すること)",
				in.Symbol, in.TakeProfit, in.StopLoss)
		}
		if pos.Side == order.SideSell && in.TakeProfit >= in.StopLoss {
			return "", fmt.Errorf("守りの値段変更 %s: 売り建玉なのに TP(%.1f)が SL(%.1f)以上 — "+
				"**取り消さない**(売りは TP が下・SL が上)",
				in.Symbol, in.TakeProfit, in.StopLoss)
		}
	}

	// ── ④ 取り消す守りが実在すること ────────────────────────────
	orders, err := r.broker.ListProtectiveOrders(ctx, in.Symbol)
	if err != nil {
		return "", fmt.Errorf("守りの値段変更 %s: 注文照会に失敗 — **取り消さない**: %w", in.Symbol, err)
	}
	var target *port.ProtectiveOrderInfo
	for i := range orders {
		if orders[i].HasStopLeg && orders[i].Side == pos.Side.Opposite() {
			target = &orders[i]
			break
		}
	}
	if target == nil {
		// 🛑 黙って新規に置かない。照会が取りこぼしただけなら守りが二重になる。
		return "", fmt.Errorf("守りの値段変更 %s: 板に逆指値の守りが見つからない — 変更ではなく**新規設置**が要る。"+
			"`POST /api/live/protective/arm` を使うこと", in.Symbol)
	}

	// ── ⑤ 呼値の格子 ─────────────────────────────────────────
	// adapter は値段を丸めない(丸めると格子外を黙ってすり替える fail-open)。格子外は
	// 立花に「逆指値条件に誤りがあります」で拒否される — それが取消の**後**に起きると裸。
	if !market.IsTickAlignedOf(in.Symbol, in.StopLoss) {
		return "", fmt.Errorf("守りの値段変更 %s: SL %g が呼値の格子に載っていない(刻み %g)— "+
			"**取り消さない**", in.Symbol, in.StopLoss, market.TickSizeOf(in.Symbol, in.StopLoss))
	}
	if in.TakeProfit > 0 && !market.IsTickAlignedOf(in.Symbol, in.TakeProfit) {
		return "", fmt.Errorf("守りの値段変更 %s: TP %g が呼値の格子に載っていない(刻み %g)— "+
			"**取り消さない**", in.Symbol, in.TakeProfit, market.TickSizeOf(in.Symbol, in.TakeProfit))
	}

	// ── ⑥ 値幅制限の帯 ────────────────────────────────────────
	// 立花は脚が 1 本でも帯の外だと**注文ごと**拒否する。SL が外なら置きに行かない
	// (行っても裸)。TP は落とすだけ(TP 欠落は機会損失、SL 欠落は致命的)。
	ocoTP := in.TakeProfit
	repriceRef := 0.0
	if r.refPrice != nil {
		if ref := r.refPrice(ctx, in.Symbol); ref > 0 {
			repriceRef = ref
			if inside, ok := risk.PriceInsideLimitBand(in.StopLoss, ref); ok && !inside {
				up, down, _ := risk.LimitBandFor(ref)
				return "", fmt.Errorf("守りの値段変更 %s: 逆指値(SL) %g が今日の値幅制限の外 "+
					"(基準値段 %g / 帯 %g〜%g)— **取り消さない**(置きに行っても注文ごと拒否され裸になる)。"+
					"帯の内側の SL を指定し直すこと", in.Symbol, in.StopLoss, ref, down, up)
			}
		}
	}
	// 多日建玉は帯の内外に依らず **stop-only**(risk.ProtectiveTakeProfitOnBoard)。
	if place, reason := risk.ProtectiveTakeProfitOnBoard(pos.HoldingMode, pos.Side.Opposite(), ocoTP, repriceRef); !place && ocoTP > 0 {
		if r.log != nil {
			r.log("守りの値段変更: TP 脚は板に載せない(SL のみ)", "symbol", in.Symbol, "tp", ocoTP, "reason", reason,
				"理由", "多日の守りは stop-only(繰越時に TP が帯の外だと SL ごと失効する)。TP は OnTick が持つ")
		}
		ocoTP = 0
	}

	// 期日は取消 → 再発注で天井がリセットされるので、9 営業日先を張り直す。
	expireOn := r.hours.NthTradingDayFrom(now, settleMaxTradingDays)
	if expireOn.IsZero() {
		return "", fmt.Errorf("🚨 守りの値段変更 %s: 休場カレンダーが尽きていて注文期日を出せない — "+
			"**取り消さない**。hard_limits の holidays / calendar_through を更新すること", in.Symbol)
	}

	// ── ここから先が裸の窓 ────────────────────────────────────
	if err := r.broker.CancelProtectiveOrder(ctx, *target); err != nil {
		// 🛑 取消が失敗 = 守りは板に残っている。現状維持が正しいので trip しない。
		return "", fmt.Errorf("守りの値段変更 %s(注文 %s): 取消に失敗 — "+
			"板の守りはそのまま残っている(TP %.1f / SL %.1f): %w",
			in.Symbol, target.OrderID, target.LimitPrice, target.StopTrigger, err)
	}

	id, err := r.broker.PlaceSettleOCO(ctx, port.OCOCloseOrderInput{
		Symbol:           in.Symbol,
		BrokerPositionID: pos.BrokerPositionID,
		Side:             pos.Side.Opposite(),
		Quantity:         pos.Quantity,
		TakeProfit:       ocoTP,
		StopLoss:         in.StopLoss,
		ExecKind:         pos.ExecKind, // 🚨 ゼロ値だと現物区分で拒否される
		ExpireOn:         expireOn,
	})
	if err != nil {
		// 🚨 取消は通ったのに置けない = **この建玉はいま裸**。
		if r.emergency != nil {
			_ = r.emergency.Trip("protective_reprice_failed:"+in.Symbol, now)
		}
		return "", fmt.Errorf("🚨🚨 守りの値段変更 %s(旧注文 %s): 取消は通ったが再発注に失敗 — "+
			"**この建玉はいま broker 側の守りが無い**。emergency を trip した。"+
			"復旧: `POST /api/live/protective/arm {\"symbol\":\"%s\",\"take_profit\":%.1f,\"stop_loss\":%.1f}` "+
			"か証券アプリから制度信用の返済注文を置くこと(旧値は TP %.1f / SL %.1f): %w",
			in.Symbol, target.OrderID, in.Symbol, in.TakeProfit, in.StopLoss,
			target.LimitPrice, target.StopTrigger, err)
	}
	if r.log != nil {
		r.log("守りの値段を変更した(取消→再発注)", "symbol", in.Symbol,
			"old_order", target.OrderID, "new_order", id,
			"old_tp", target.LimitPrice, "old_sl", target.StopTrigger,
			"tp", ocoTP, "sl", in.StopLoss,
			"expire_on", expireOn.Format("2006-01-02"))
	}
	// ── 台帳を板と同じ値へ ─────────────────────────────
	// 🚨 板だけ変えると、守りが消えたとき RearmUnguarded が**古い凍結値**で置き直し、
	// 多日の TP は OnTick が古い値で判定する(4901: 板 SL 3115 / 台帳 2986.5 のまま 3 週間)。
	// TP 0 は「板に利確脚を載せない」の意味なので台帳の利確は据え置く(消すと戦略の出口が変わる)。
	// 🛑 失敗しても板は変更済み — trip はせず、食い違いを名指しで返す。
	tp, tpJPY := pos.TakeProfitPrice, pos.TakeProfitJPY
	if in.TakeProfit > 0 {
		tp, tpJPY = in.TakeProfit, math.Abs(in.TakeProfit-pos.EntryPrice)
	}
	ok, err := r.positions.UpdateProtectivePrices(ctx, pos.ID, tp, in.StopLoss, tpJPY, math.Abs(pos.EntryPrice-in.StopLoss))
	if err == nil && !ok {
		err = errors.New("建玉が OPEN でない(決済中 / 決済済み)")
	}
	if err != nil {
		return id, fmt.Errorf("守りの値段変更 %s: **板は変更済み**(新注文 %s・TP %.1f / SL %.1f)だが台帳の更新に失敗 — "+
			"板と台帳が食い違っている。live DB の positions id=%d を同じ値へ揃えること: %w",
			in.Symbol, id, tp, in.StopLoss, pos.ID, err)
	}
	return id, nil
}
