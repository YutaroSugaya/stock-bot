// Package command holds the state-changing usecases (CQRS write side).
package command

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// ExecuteOrder runs the entry saga. The protective exit is placed BROKER-side so
// the守り survives bot death; any failure after the fill compensates by closing
// the naked position.
type ExecuteOrder struct {
	broker    port.LiveBroker
	posRepo   port.PositionRepository
	pending   port.PendingPositionTracker
	closer    port.PositionCloser // optional: nil なら巻き戻しを台帳に書けない(後述)
	emergency EmergencyController
	clock     clock.Clock
	Ops       OpsCounters // optional, nil-safe (observability only)

	// 🛑 entry saga を**約定の後に**巻き戻した銘柄と、その営業日。同じ銘柄で
	// 買い→売りの往復を同日に繰り返さないための記憶。補償で閉じた事実は台帳に何も
	// 書かないので、再入場を止めうるゲート(ナンピン禁止 / 建玉数 / 日次損失 /
	// 連敗)は**全部が台帳を読む以上、構造的に効かない**。台帳の外に覚える。
	// 銘柄単位に留めるのは、1 銘柄の事情で口座全体の live を止めないため
	// (全体停止は emergency_stop の担当)。日付で自然に解けるのは、値幅制限が
	// 基準値段で決まる日単位の事象だから。
	rollbackMu   sync.Mutex
	rolledBackOn map[string]string // symbol -> YYYY-MM-DD
}

// NewExecuteOrder wires the saga. em trips emergency_stop when a compensating
// close fails, so it must not be nil in production.
func NewExecuteOrder(b port.LiveBroker, pr port.PositionRepository, pending port.PendingPositionTracker, em EmergencyController, c clock.Clock) *ExecuteOrder {
	if c == nil {
		c = clock.System()
	}
	return &ExecuteOrder{broker: b, posRepo: pr, pending: pending, emergency: em, clock: c}
}

// WithCloser wires the close saga so a **rolled-back entry is booked in the
// ledger**(close_reason='entry_compensated')。無いと巻き戻した往復が台帳に 1 行も
// 残らず、cooldown / max_trades_in_this_window / daily_loss / account_daily_loss /
// consecutive_losses が**同時に**盲目になる(同日に同じ銘柄で往復を繰り返す原因)。
func (e *ExecuteOrder) WithCloser(c port.PositionCloser) *ExecuteOrder {
	e.closer = c
	return e
}

// ExecuteOrderInput is an approved entry (the risk gate already passed).
type ExecuteOrderInput struct {
	Signal   strategy.Signal
	Quantity int // post-multiplier quantity
	ExecKind order.ExecKind
	Source   position.Source

	// OCOExpireOn は broker 側の守りをいつまで板に残すか。zero = 当日限り。
	// 決めるのは休場カレンダーを持つ呼び出し側(TradingCycle)で、ここは運ぶだけ。
	OCOExpireOn time.Time

	// PriceLimitRef は値幅制限の基準値段(前日終値)。0 = 判定しない(backtest 等)。
	// 日足を持つ呼び出し側が渡す。ChainLinkSplits 済みの終値であること。
	PriceLimitRef float64
}

func (e *ExecuteOrder) Execute(ctx context.Context, in ExecuteOrderInput) (int64, error) {
	sig := in.Signal
	if !sig.IsEntry() {
		return 0, fmt.Errorf("execute_order: signal is not an entry")
	}
	if in.Quantity <= 0 {
		return 0, fmt.Errorf("execute_order: non-positive quantity %d", in.Quantity)
	}

	// Pending for the WHOLE saga (the broker position id is unknown until the fill
	// resolves): Reconcile skips a pending symbol and cannot mis-adopt the
	// in-flight fill as external.
	e.pending.MarkPendingSymbol(sig.Symbol)
	defer e.pending.MarkResolvedSymbol(sig.Symbol)

	placed, err := e.broker.PlaceOrder(ctx, order.PlaceOrderRequest{
		Symbol: sig.Symbol, Side: sig.Side, Type: order.OrderTypeMarket,
		Quantity: in.Quantity, ExecKind: in.ExecKind, ClientTag: sig.SignalID,
	})
	if err != nil {
		// Transport error = UNCONFIRMABLE: the request may have reached the exchange
		// and filled. Never assume it didn't — verify, cancel/compensate, and trip
		// when verification itself fails.
		e.recoverUnconfirmedSubmit(ctx, sig, in)
		return 0, fmt.Errorf("execute_order: place: %w", err)
	}
	if !placed.Accepted {
		// Confirmed business reject — nothing reached the book.
		return 0, fmt.Errorf("execute_order: entry rejected: %s", placed.Message)
	}

	// An UNCONFIRMED fill must not be left as a possibly-open, unprotected,
	// untracked position: compensate (close) rather than assume nothing filled.
	rex, err := e.broker.ResolveExecution(ctx, placed.OrderID)
	if err != nil {
		if errors.Is(err, port.ErrOrderNotFilled) {
			// CONFIRMED zero fill (ストップ高/安・売買停止・薄商い): no position exists, so
			// cancel the working order and abort — do NOT compensate or trip emergency
			// **when the cancel is confirmed**. An unconfirmed cancel leaves a live
			// order that can fill later, naked and untracked.
			rex, err = e.cancelUnfilledEntry(ctx, placed.OrderID, sig, err)
			if err != nil {
				return 0, err
			}
			// The residual filled between the resolve and the cancel: protect it like
			// any other fill by falling through.
		} else {
			e.compensate(ctx, "", placed.BrokerPositionID, sig, in.Quantity, in.ExecKind, fillContext{}) // no OCO placed yet
			return 0, fmt.Errorf("execute_order: resolve fill failed, position closed: %w", err)
		}
	}
	bpID := placed.BrokerPositionID
	if bpID == "" {
		bpID = rex.BrokerPositionID
	}
	e.pending.MarkPending(bpID)
	defer e.pending.MarkResolved(bpID)

	// 約定数量が正: a partial fill must not freeze — or protect — the REQUESTED
	// quantity, or the OCO over-sells and every downstream risk calc is wrong.
	qty := rex.FilledQuantity
	if qty <= 0 {
		qty = in.Quantity // adapter did not report a quantity; requested == filled
	}
	if qty < in.Quantity {
		// Cancel the residual so a later fill cannot create an unprotected, untracked
		// add-on lot. The cancel OUTCOME is authoritative: discarding it (`_, _ =`)
		// let a failed cancel proceed to protect the wrong quantity while the residual
		// could still fill — invisible to reconcile, which sees only 立花's aggregate
		// 建玉 id.
		cres, cerr := e.broker.CancelOrder(ctx, placed.OrderID)
		cancelled := cerr == nil && cres != nil && cres.Cancelled
		// Re-resolve regardless of the cancel result: a fill can land between the
		// resolve snapshot and the cancel taking effect.
		if rex2, rerr := e.broker.ResolveExecution(ctx, placed.OrderID); rerr == nil && rex2.FilledQuantity > qty {
			qty = rex2.FilledQuantity
			if rex2.FilledPrice > 0 {
				rex.FilledPrice = rex2.FilledPrice
			}
			if rex2.FeeJPY > 0 {
				rex.FeeJPY = rex2.FeeJPY
			}
		}
		// 🛑 The cancel verdict is evaluated AFTER the re-resolve, not in an else
		// branch: "more shares filled but still partial" with a failed cancel used to
		// skip this check and freeze the larger partial while the residual kept
		// working.
		if qty < in.Quantity && !cancelled {
			// Residual neither cancelled nor confirmably filled: never protect a guessed
			// quantity — close the filled portion and trip, surfacing it to a human.
			e.compensate(ctx, "", bpID, sig, qty, in.ExecKind, fillContext{entryPrice: rex.FilledPrice, entryFee: rex.FeeJPY}) // no OCO placed yet
			if e.emergency != nil {
				_ = e.emergency.Trip("partial_fill_residual_uncancelled:"+sig.Symbol, e.clock())
			}
			return 0, fmt.Errorf("execute_order: partial-fill residual cancel unconfirmed (filled %d of %d), position closed and tripped: %w", qty, in.Quantity, cerr)
		}
	}

	// TP/SL の絶対価格は約定値が確定してはじめて決まる(config は建値からの円幅で
	// 持つ — arm から entry まで時間が空くので絶対価格では値動きでずれる)。
	// 呼値グリッドへの丸めはここだけ。
	entryPrice := rex.FilledPrice
	tickSize := market.TickSizeOf(sig.Symbol, entryPrice)
	fill := fillContext{entryPrice: entryPrice, entryFee: rex.FeeJPY, tickSize: tickSize}
	tp, sl := position.TPSLPricesFromJPY(sig.Symbol, sig.Side, entryPrice, sig.TakeProfitJPY, sig.StopLossJPY)

	// 🛑 本日の値幅制限の外に出る利確指値は board に置けない(立花は注文ごと拒否し、
	// 約定済みの建玉が守り無しで残る)。**TP 脚だけ落として
	// SL を置く** — 立花の PlaceSettleOCO は TakeProfit=0 で「逆指値のみ」を受ける。
	// TP は台帳(take_profit_price)に残るので ManageOpenPositions.OnTick が引き継ぐ。
	// CLAUDE.md「SL 欠落は致命的、TP 欠落は機会損失のみ」に沿った縮退。
	// PriceLimitRef が 0(判定材料なし)なら**何も変えない** — 判定できないことを
	// 理由に守りの形を勝手に変えない。
	// 🚨 落とすのは **board に出す脚だけ**。台帳の take_profit_price は必ず残す。
	// ここを 1 つの変数で兼ねると、TP を落とした建玉は台帳にも 0 で凍結され、
	// **broker 側にも bot 側にも TP が無い**建玉になる。
	// price_limit_gate.go / CLAUDE.md / FAILURE_MODES.md が約束する
	// 「TP は OnTick が引き継ぐ」は、台帳に値が残っていて初めて成立する。
	// **多日建玉は帯の内外に依らず stop-only**(risk.ProtectiveTakeProfitOnBoard)。
	// 立花は繰越時に翌日の帯で再検査し、TP 脚が外だと SL ごと失効させるため。intraday は両脚。
	ocoTP := tp
	if place, reason := risk.ProtectiveTakeProfitOnBoard(sig.HoldingMode, sig.Side.Opposite(), tp, in.PriceLimitRef); !place {
		ocoTP = 0
		if reason == "outside_price_limit" && e.Ops != nil {
			e.Ops.IncrProtectiveTPDropped() // 帯による縮退だけを数える(多日の stop-only は方針であって縮退ではない)
		}
	}

	rootOCO, err := e.broker.PlaceSettleOCO(ctx, port.OCOCloseOrderInput{
		Symbol: sig.Symbol, BrokerPositionID: bpID, Side: sig.Side.Opposite(),
		Quantity: qty, TakeProfit: ocoTP, StopLoss: sl, ExecKind: in.ExecKind,
		ExpireOn: in.OCOExpireOn,
	})
	if err != nil {
		e.compensate(ctx, "", bpID, sig, qty, in.ExecKind, fill) // OCO placement failed → nothing resting
		return 0, fmt.Errorf("execute_order: place OCO failed, position closed: %w", err)
	}
	// From here the protective OCO is RESTING and 拘束 the 建玉 quantity, so every
	// compensating close must cancel rootOCO first or the broker rejects it.
	if _, _, err := e.broker.ResolveSettleLegs(ctx, bpID, ""); err != nil {
		e.compensate(ctx, rootOCO, bpID, sig, qty, in.ExecKind, fill)
		return 0, fmt.Errorf("execute_order: resolve legs failed, position closed: %w", err)
	}
	// 🛑 **板に本当に乗ったか**を broker に訊く。直前の ResolveSettleLegs は立花では
	// プロセス内 map を読むだけ(= 「置いたと覚えている」)で、PlaceSettleOCO が成功を
	// 返しさえすれば注文が実在しなくても素通りする。それは CLAUDE.md「TP/SL は必ず
	// broker 側」を満たしていない — 無防備な建玉が残るのはこの経路。
	// 照会失敗も拒否(fail-close): 確かめられないことを大丈夫と読まない。
	if err := verifyProtectiveOrder(ctx, e.broker, symbolOf(sig), sig.Side.Opposite(), qty); err != nil {
		e.compensate(ctx, rootOCO, bpID, sig, qty, in.ExecKind, fill)
		return 0, fmt.Errorf("execute_order: %w — position closed", err)
	}

	// config 凍結: TP/SL/MaxHold/ratchet/HoldingMode/ExecKind をここで固定するので、
	// 以後の active config 切替は既存ポジに影響しない。
	entryFee := rex.FeeJPY
	posID, err := e.posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: bpID, Symbol: symbolOf(sig), Side: sig.Side, Quantity: qty, EntryPrice: entryPrice,
		TakeProfitJPY: sig.TakeProfitJPY, StopLossJPY: sig.StopLossJPY,
		TakeProfitPrice: tp, StopLossPrice: sl, MaxHoldMinutes: sig.MaxHoldMinutes,
		ExtensionMaxMinutes: sig.ExtensionMaxMinutes, ExtensionUnrealizedJPY: sig.ExtensionUnrealizedJPY,
		EarlyExitWindowMinutes: sig.EarlyExitWindowMinutes, EarlyExitTargetJPY: sig.EarlyExitTargetJPY,
		RatchetArmJPY: sig.RatchetArmJPY, RatchetGivebackJPY: sig.RatchetGivebackJPY,
		// トレールの床は**新規建玉だけ**に効く。床の導入前に建てた旧建玉は
		// false のまま(DB の default)= 旧規則で終わる。
		// ここを落とすと床は何も変わらない(全建玉が旧規則)。
		RatchetFloorAtArm: true,
		StrategyConfigID:  sig.ConfigID,
		// ナンピン禁止のキーが (銘柄, 側, 戦略) になったので戦略名も凍結する。
		// 落とすと "" = 戦略不明になり、**同一銘柄の全戦略が互いをブロックする**
		// (安全側だがペアが 1 本も建たない)。
		StrategyName: string(sig.StrategyName),
		HoldingMode:  sig.HoldingMode, ExecKind: in.ExecKind,
		TickSizeAtEntry: tickSize, Source: in.Source, EntryFeeJPY: &entryFee, OpenedAt: e.clock(),
	})
	if err != nil {
		e.compensate(ctx, rootOCO, bpID, sig, qty, in.ExecKind, fill)
		return 0, fmt.Errorf("execute_order: insert position failed, position closed: %w", err)
	}
	return posID, nil
}

// compensate closes a naked broker position when a post-fill step fails. A
// RESTING protective OCO (protectiveOrderID non-empty) is cancelled FIRST, else
// its 拘束 makes the broker reject the close and orphan the position. When the
// compensating close ITSELF fails the broker holds an unprotected position the
// bot cannot see, so it trips emergency_stop rather than fail open.
// fillContext is what compensate needs to BOOK the rolled-back round trip. Price 0
// = 約定値が確定していない(ResolveExecution が失敗した枝)→ 台帳には書かない。
// 値を捏造するくらいなら書かない、という向きに倒す。
type fillContext struct {
	entryPrice float64
	entryFee   float64
	tickSize   float64
}

func (e *ExecuteOrder) compensate(ctx context.Context, protectiveOrderID, bpID string, sig strategy.Signal, qty int, execKind order.ExecKind, fill fillContext) {
	if e.Ops != nil {
		e.Ops.IncrCompensations()
	}
	// 🛑 台帳の外に「この銘柄は今日もう建てない」を刻む。**補償で閉じた往復は台帳に
	// 1 行も残らない**ので、これを置かないと次のティックで全ゲートが通り、同じ失敗を
	// 繰り返す(実弾で同じ銘柄を何往復もしうる)。
	e.blockSymbolForDay(symbolOf(sig))
	if protectiveOrderID != "" {
		_, _ = e.broker.CancelOrder(ctx, protectiveOrderID) // release the reservation before closing
	}
	res, err := e.broker.ClosePosition(ctx, port.CloseRequest{
		Symbol: symbolOf(sig), BrokerPositionID: bpID, Side: sig.Side.Opposite(), Quantity: qty, ExecKind: execKind,
	})
	if err != nil || res == nil || !res.Accepted {
		if e.emergency != nil {
			_ = e.emergency.Trip("compensating_close_failed:"+bpID, e.clock())
		}
		return
	}
	e.bookCompensatedRoundTrip(ctx, sig, bpID, qty, execKind, fill, res)
}

// bookCompensatedRoundTrip records the entry that really happened and the close
// that unwound it. **Any early return here loses the ledger row, not money** —
// the position is already closed at the broker — so every branch is a silent
// skip rather than a failure of the saga.
//
// 🚨 受理は約定ではない(立花は決済約定が非同期)。resolveSettleFill で確認できた
// ときだけ trade を書き、確認できなければ **CLOSING のまま**残して reconcile に渡す
// (幽霊決済を書かない・裸の建玉を見失わない、の両立)。
func (e *ExecuteOrder) bookCompensatedRoundTrip(ctx context.Context, sig strategy.Signal, bpID string,
	qty int, execKind order.ExecKind, fill fillContext, res *port.CloseResult) {
	if e.closer == nil || fill.entryPrice <= 0 || qty <= 0 {
		return // 約定値が確定していない枝では書かない(捏造しない)
	}
	now := e.clock()
	entryFee := fill.entryFee
	posID, err := e.posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: bpID, Symbol: symbolOf(sig), Side: sig.Side, Quantity: qty, EntryPrice: fill.entryPrice,
		TakeProfitJPY: sig.TakeProfitJPY, StopLossJPY: sig.StopLossJPY,
		StrategyConfigID: sig.ConfigID, HoldingMode: sig.HoldingMode, ExecKind: execKind,
		// 🚨 **戦略名を凍結する**。ここだけ落ちていた。決済が確認できず
		// CLOSING で座礁すると `strategy_name = ''` の建玉が残り、ナンピン禁止も
		// arm 判定も「戦略不明 = 全戦略を建玉中とみなす」に倒れるので、
		// **その銘柄の全アームが無期限にブロックされる**。
		StrategyName: string(sig.StrategyName),
		// 巻き戻した往復も新規建玉なので床の版スタンプは true。
		RatchetFloorAtArm: true,
		TickSizeAtEntry:   fill.tickSize, Source: position.SourceBot, EntryFeeJPY: &entryFee, OpenedAt: now,
	})
	if err != nil {
		return
	}
	// CLOSING へ落としてから close saga に渡す(CAS の作法は通常の決済と同じ)。
	if ok, cerr := e.posRepo.ClaimForClose(ctx, posID, now); cerr != nil || !ok {
		return
	}
	closePrice, settleFee, confirmed := resolveSettleFill(ctx, e.broker, res, qty)
	if !confirmed || closePrice <= 0 {
		// 🚨 補償の巻き戻しも **守りを cancel した後**に決済を撃っている
		// (entry saga は建玉に OCO を置いてから失敗しうる)。決済が受理されたのに
		// 約定も板への常駐も確認できなければ、この建玉は逆指値も決済注文も持たない
		// = 完全に裸。closeOne が塞いでいるのと**同じ形の穴**がこちらにもある。
		// 🛑 述語もガードも closeOne と**共有する** — 同じロジックを 2 箇所に書くと、
		// 片方だけ直して緑のまま穴が残る。
		tripIfSettleLeavesPositionNaked(ctx, e.broker, e.emergency,
			symbolOf(sig), sig.Side.Opposite(), bpID, res, now)
		return // CLOSING のまま = reconcile が引き継ぐ
	}
	p := position.Position{Side: sig.Side, Quantity: qty, EntryPrice: fill.entryPrice}
	_, _ = e.closer.CloseAndRecord(ctx, posID, now, port.TradeRecord{
		PositionID: posID, Symbol: symbolOf(sig), Side: sig.Side, Quantity: qty,
		EntryPrice: fill.entryPrice, ClosePrice: closePrice, ProfitLossJPY: grossPnL(p, closePrice),
		FeeJPY: entryFee + settleFee, CloseReason: port.CloseReasonEntryCompensated, ClosedAt: now,
	})
}

// recoverUnconfirmedSubmit handles a PlaceOrder transport failure. The submit
// path has no idempotency key, so "assume it failed" can orphan a filled,
// unprotected, untracked position: verify against the broker book instead —
// cancel a matching working order, and compensate away a matching UNKNOWN
// same-side position (the nanpin gate guarantees the bot would not have entered
// over an existing one). Verification failure trips emergency, never fail-open.
func (e *ExecuteOrder) recoverUnconfirmedSubmit(ctx context.Context, sig strategy.Signal, in ExecuteOrderInput) {
	trip := func() {
		if e.emergency != nil {
			_ = e.emergency.Trip("entry_submit_unconfirmed:"+sig.Symbol, e.clock())
		}
	}
	orders, err := e.broker.GetActiveOrders(ctx, sig.Symbol)
	if err != nil {
		trip()
		return
	}
	cancelFailed := false
	for _, o := range orders {
		if o.Side == sig.Side && o.Quantity == in.Quantity {
			if res, cerr := e.broker.CancelOrder(ctx, o.OrderID); cerr != nil || res == nil || !res.Cancelled {
				cancelFailed = true // may have just filled — the position scan below decides
			}
		}
	}
	known := make(map[string]struct{})
	if list, lerr := e.posRepo.ListOpenOrClosing(ctx, sig.Symbol); lerr == nil {
		for _, p := range list {
			if p.BrokerPositionID != "" {
				known[p.BrokerPositionID] = struct{}{}
			}
		}
	}
	positions, err := e.broker.GetPositions(ctx)
	if err != nil {
		trip()
		return
	}
	for _, bp := range positions {
		if bp.Symbol != sig.Symbol || bp.Side != sig.Side {
			continue
		}
		if _, ok := known[bp.BrokerPositionID]; ok {
			continue // tracked position (e.g. pyramiding config) — not ours to touch
		}
		// 🚨 **我々が作りえた孤児は多くても発注数量ぶん**。それより大きい建玉は「我々の
		// ものだけではない」— 立花は信用建玉を**銘柄単位に集約**する(broker_position_id は
		// "shinyo:<symbol>" 1 本)ので、丸ごと閉じると**人間が手で建てた玉まで売る**。
		// 台帳が空のまま人間が同じ銘柄を手動で信用買いしている状態はありうる。
		// 数量から所有を証明できないので**推測せず trip** して人間に渡す。裸の孤児は残るが、
		// 他人の建玉を売る方が取り返しがつかない(reconcile が external として拾い、
		// 守りが無いことも別途 trip で surface する)。
		if bp.Quantity > in.Quantity {
			trip()
			return
		}
		e.compensate(ctx, "", bp.BrokerPositionID, sig, bp.Quantity, in.ExecKind, fillContext{entryPrice: bp.EntryPrice}) // orphan fill, no OCO placed
		return
	}
	if cancelFailed {
		trip() // cancel failed AND no fill visible — cannot account for the order
	}
}

// cancelUnfilledEntry handles a CONFIRMED zero fill. The cancel OUTCOME is
// authoritative (the same rule as the partial-fill path): a confirmed cancel is
// the clean abort; an unconfirmed one is re-resolved once, and if the order is
// still neither cancelled nor filled the saga trips and blocks the symbol for the
// day — the order may still fill, and nothing else would notice.
//
// Returns the fill when the residual filled meanwhile (the caller protects it), or
// the error to abort with.
func (e *ExecuteOrder) cancelUnfilledEntry(ctx context.Context, orderID string, sig strategy.Signal, notFilled error) (port.ResolvedExecution, error) {
	cres, cerr := e.broker.CancelOrder(ctx, orderID)
	if cerr == nil && cres != nil && cres.Cancelled {
		return port.ResolvedExecution{}, fmt.Errorf("execute_order: entry not filled, order cancelled: %w", notFilled)
	}
	rex2, rerr := e.broker.ResolveExecution(ctx, orderID)
	if rerr == nil && rex2.FilledQuantity > 0 {
		return rex2, nil
	}
	e.blockSymbolForDay(symbolOf(sig))
	if e.emergency != nil {
		_ = e.emergency.Trip("entry_not_filled_cancel_unconfirmed:"+symbolOf(sig), e.clock())
	}
	return port.ResolvedExecution{}, fmt.Errorf("execute_order: entry not filled and cancel unconfirmed (order %s may still be working), tripped: cancel=%v resolve=%v: %w",
		orderID, cerr, rerr, notFilled)
}

func symbolOf(sig strategy.Signal) string { return sig.Symbol }

func (e *ExecuteOrder) blockSymbolForDay(symbol string) {
	if symbol == "" {
		return
	}
	e.rollbackMu.Lock()
	defer e.rollbackMu.Unlock()
	if e.rolledBackOn == nil {
		e.rolledBackOn = make(map[string]string, 4)
	}
	e.rolledBackOn[symbol] = e.clock().Format("2006-01-02")
}

// EntryBlocked reports whether this symbol already rolled back a FILLED entry
// today. 呼び手(TradingCycle)は建てる前にこれを見る。
func (e *ExecuteOrder) EntryBlocked(symbol string, now time.Time) bool {
	e.rollbackMu.Lock()
	defer e.rollbackMu.Unlock()
	day, ok := e.rolledBackOn[symbol]
	return ok && day == now.Format("2006-01-02")
}
