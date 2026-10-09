package command

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// TradingCycle is the per-tick ENTRY path; exits live in ManageOpenPositions.
type TradingCycle struct {
	engine      *strategy.Engine
	snapshot    *SnapshotBuilder
	executor    *ExecuteOrder
	minQty      int
	execKindFor func(order.HoldingMode) order.ExecKind
	hours       session.TradingHours

	// Boundary is the per-trade fat-finger ceiling. ZERO-VALUED DISABLES EVERY
	// CHECK — production wiring must set it from hard_limits.
	Boundary risk.OrderBoundary

	// Rejections is the restart-surviving "なぜエントリーしなかったか" trail. Optional
	// (nil-safe) and best-effort: a failed insert never blocks the cycle.
	Rejections port.SignalRejectionRepository

	// Loanable は貸借銘柄ホワイトリスト(hard_limits.loanable_symbols)。
	// 🛑 **zero 値 = 売りを全 reject**(fail-close)。買いには効かない。
	// 配線を忘れても売り側の標本はゼロになるが、理由(`loanable_list_missing`)が
	// signal_rejections に残るので後から原因にたどり着ける。
	Loanable risk.LoanableSymbols

	// EntryLock は**口座(トラック)単位**の entry 排他。snapshot の取得から発注までを囲む。
	// nil = 直列化しない(単体テスト用)。🛑 production は必ず挿す — 挿し忘れると口座の
	// 枠を 2 銘柄が同時に取れる(`TestBuildSymbolBundle_SharesOneEntryLockPerTrack`)。
	EntryLock *EntrySerializer

	// lastReject dedups signal_rejections down to state-change edges. Without it
	// every tick rewrote the same reason — 実測 233,193 行。エッジは
	// (種別, config, JST日) のいずれかが変わったとき。日を含めるのは週跨ぎの建玉で
	// 月曜が無記録になる穴を塞ぐため。per-bundle の price goroutine から直列に
	// 呼ばれる前提(lock なし)。
	//
	// 🛑 キーは **(銘柄, 戦略)**。1 銘柄に複数アームが載るようになったので、
	// 銘柄キーのままだと 2 アームが**互いの記録を上書きし合い**、毎ティック 2 行書かれる
	// (= 233,193 行事故の再来)。逆に片方の理由が消える取りこぼしも起きる。
	lastReject map[rejectKey]rejectEdge
	// 🚨 **見送りの記録は別のエッジで覚える**。
	//
	// `lastReject` は (銘柄, 戦略) につきエッジを 1 つしか持たないので、**2 種類の理由が
	// 交互に来ると毎ティック「変化した」と判定**される。コスト床の入力は**その瞬間の
	// 気配**(`CurrentRate.SpreadTicks`)で、比較先は **ATR 由来で日中不変** —— つまり
	// スプレッドが 2↔3 ティック揺れるだけで `tp_below_cost_floor` と entry 提案が交互に
	// 出る。建玉済みなら entry 側は gate が `open_positions` で落とすので、
	// **3 秒ごとに 1 行**が入る(= 233,193 行事故の再来)。
	//
	// 系列を分ければ干渉しない。事前登録が数えたいのは「その日そのアームで床に落ちたか」
	// なので、(銘柄, 戦略, JST日)に 1 行で必要十分。
	lastNoTrade map[rejectKey]rejectEdge

	// reconcileHeld は「この銘柄は起動時 reconcile がまだ 1 回も成功していない」(D-1)。
	// 立てるのは起動時の同期 reconcile の直前だけで、落とすのは reconcile の成功だけ。
	// reconcile レーンと price レーンは別 goroutine なので atomic で持つ。
	reconcileHeld atomic.Bool
}

// ReasonReconcileUnconfirmed は起動時 reconcile が成功するまで新規 entry を保留した理由。
//
// 🚨 **再起動直後に broker 照会が落ちると、台帳は broker の建玉を知らない**。
// そのまま entry を通すとナンピン禁止ゲートが既存建玉を
// 見落として二重に建てる。起動自体は止めない — 守りの置き直しと決済は動かし続ける。
const ReasonReconcileUnconfirmed = "reconcile_unconfirmed"

// MinCollateralJPY は entry 経路が使う最低委託保証金(配線の検査用)。
func (c *TradingCycle) MinCollateralJPY() int {
	if c.snapshot == nil {
		return 0
	}
	return c.snapshot.caps.MinCollateralJPY
}

// AccountMaxEntriesPerDay は entry 経路の snapshot に届いた「1 営業日の新規本数」の上限(配線の検査用)。
func (c *TradingCycle) AccountMaxEntriesPerDay() int {
	if c.snapshot == nil {
		return 0
	}
	return c.snapshot.caps.AccountMaxEntriesPerDay
}

// HoldEntriesUntilReconciled は reconcile が 1 回成功するまで新規 entry を保留する。
func (c *TradingCycle) HoldEntriesUntilReconciled() { c.reconcileHeld.Store(true) }

// MarkReconciled は reconcile の成功で保留を解く。
func (c *TradingCycle) MarkReconciled() { c.reconcileHeld.Store(false) }

// EntriesHeldForReconcile は保留中か(観測・テスト用)。
func (c *TradingCycle) EntriesHeldForReconcile() bool { return c.reconcileHeld.Load() }

type rejectKey struct {
	symbol   string
	strategy config.StrategyName
}

type rejectEdge struct{ kind, configID, day string }

func NewTradingCycle(eng *strategy.Engine, snap *SnapshotBuilder, exec *ExecuteOrder, minQty int, execKindFor func(order.HoldingMode) order.ExecKind, hours session.TradingHours) *TradingCycle {
	if execKindFor == nil {
		execKindFor = func(order.HoldingMode) order.ExecKind { return order.ExecMarginOneday }
	}
	return &TradingCycle{engine: eng, snapshot: snap, executor: exec, minQty: minQty, execKindFor: execKindFor, hours: hours}
}

type TradingCycleResult struct {
	Signal     strategy.Signal
	Entered    bool
	PositionID int64
	// "" when entered, or when the strategy proposed nothing at all.
	// 🔄 **入口成立後にコスト床で断った見送り**(recordableNoTradeReasons)もここに入る
	// — あれは「提案が無かった」ではなく「提案を我々が断った」なので、reject の
	// 件数にもログにも出るのが正しい。
	RejectReason string
}

// 🚨 **記録する見送り理由の allowlist**。
//
// 見送りの大半は「セットアップが無い」で、200銘柄 × 13アーム × 3秒 で回る以上、
// 全部書いたら台帳が使い物にならない(233,193 行事故)。逆に **入口は成立したのに
// 我々が断った**ものは、書かなければその瞬間に消える —— しかも床は trail 側だけ
// 3 倍厳しいので、**壊れたペアは選択的に片側から消える**。ペア差を本命の統計量に
// 据えた設計が、記録しないと成立しない。
//
// 足すのは「セットアップの不成立」ではなく「成立後の拒否」に限ること。
var recordableNoTradeReasons = map[string]bool{
	strategy.ReasonTPBelowCostFloor: true,
}

func (c *TradingCycle) Execute(ctx context.Context, in strategy.EvalInput) (TradingCycleResult, error) {
	sig := c.engine.Evaluate(in)
	res := TradingCycleResult{Signal: sig}
	if !sig.IsEntry() {
		if recordableNoTradeReasons[sig.Reason] {
			res.RejectReason = sig.Reason
			c.recordNoTrade(ctx, sig, res.RejectReason, in.Now)
		}
		return res, nil
	}

	if c.reconcileHeld.Load() {
		res.RejectReason = ReasonReconcileUnconfirmed
		c.recordRejection(ctx, sig, res.RejectReason, in.Now)
		return res, nil
	}

	// 🛑 ここから発注までが「口座の枠を見て、取る」区間。口座単位で直列化する。
	// entry を提案しないティック(大半)は排他に触れない。
	if c.EntryLock != nil {
		release, ok := c.EntryLock.acquire(ctx)
		if !ok {
			res.RejectReason = ReasonEntryLockTimeout
			c.recordRejection(ctx, sig, res.RejectReason, in.Now)
			return res, nil
		}
		defer release()
	}

	// 一日信用 can never be held multiday. Checked on the RESOLVED exec kind — the
	// gate's cfg.ExecKind check is only a secondary guard.
	execKind := c.execKindFor(sig.HoldingMode)
	if execKind == order.ExecMarginOneday && sig.HoldingMode == order.HoldingMultiday {
		res.RejectReason = "oneday_margin_cannot_hold_multiday"
		c.recordRejection(ctx, sig, res.RejectReason, in.Now)
		return res, nil
	}

	// 🛑 **現物では売れない**。exec_kind は holding_mode からしか決まらない
	// ので、`multiday.exec_kind` を cash に戻した瞬間に**空売りが 現物売り として通り、
	// carry(貸株料)が 0 で記帳される**(position/carry.go は ExecCash に 0 を返す)。
	// 「売りのコストは貸株料込みで測る」と事前登録しているので、
	// これが起きると売り標本の net が静かに甘く出る。設定ミスをここで落とす。
	if sig.Side == order.SideSell && execKind == order.ExecCash {
		res.RejectReason = "cash_cannot_short"
		c.recordRejection(ctx, sig, res.RejectReason, in.Now)
		return res, nil
	}

	// Cap an intraday MaxHold so a position can never straddle the forced flatten.
	if sig.HoldingMode == order.HoldingIntraday && c.hours.TZ != nil {
		limit := c.hours.MinutesUntilForceFlat(in.Now)
		if limit <= 0 {
			res.RejectReason = "no_time_before_force_flat"
			c.recordRejection(ctx, sig, res.RejectReason, in.Now)
			return res, nil
		}
		if sig.MaxHoldMinutes == 0 || sig.MaxHoldMinutes > limit {
			sig.MaxHoldMinutes = limit
		}
	}

	// 🛑 同じ営業日に **約定の後で巻き戻した銘柄**は、その日はもう建てない。
	// 巻き戻しは台帳に書けないことがあり(約定値が確定しない枝)、台帳を読むゲートが
	// 揃って盲目になる。executor がプロセス内に持つ記憶をここで見る。
	if c.executor != nil && c.executor.EntryBlocked(sig.Symbol, in.Now) {
		res.RejectReason = "entry_rolled_back_today"
		c.recordRejection(ctx, sig, res.RejectReason, in.Now)
		return res, nil
	}

	// 🛑 **構造ゲートを先に回し、通ったものにだけ口座照会を払う**。
	// 建玉枠・ナンピン禁止・セッションなど「我々の帳簿だけで決まる」reject に立花への
	// 3 リクエストを払わない。枠が満杯の live では armed 銘柄が毎ティック シグナルを
	// 出し続けるので、逆順(照会 → 判定)だと 1,680回/日 で API 予算を焼き切る
	// (実測・TACHIBANA_API_NOTES.md)。
	//
	// 🚨 **順序だけでは足りなかった**。`gross_notional_cap` のように
	// **照会の後ろにしか置けないゲート**で毎ティック落ちる銘柄があると、同じ焼き方が
	// そのまま再来する(実測: 後場だけで wire 数千回)。だから照会自体を
	// `AccountMarginCache` に集めて「初回 / 約定・決済 / maxAge」だけに絞ってある。
	snap := c.snapshot.BuildStructural(ctx, sig.Symbol, in.Summary, sig, execKind)
	if d := risk.EvaluateStructural(sig, in.Config, snap, in.Summary); !d.Allowed {
		res.RejectReason = d.Reason
		c.recordRejection(ctx, sig, res.RejectReason, in.Now)
		return res, nil
	}
	c.snapshot.FillCollateral(ctx, &snap, sig, execKind)
	// 構造ゲートを**もう一度**通す形でフル版を呼ぶ。純粋計算なので費用はゼロで、
	// 「最終判定は必ずフルゲート」という不変条件が配線ミスに依らず保たれる。
	decision := risk.EvaluateSignal(sig, in.Config, snap, in.Summary)
	if !decision.Allowed {
		res.RejectReason = decision.Reason
		c.recordRejection(ctx, sig, res.RejectReason, in.Now)
		return res, nil
	}

	qty := applyQtyMultiplier(sig.Quantity, decision.QtyMultiplier, c.minQty)

	// 🛑 値幅制限(ストップ高/安)。**約定の後**に守りの発注が拒否されると、bot は
	// 約定済みの建玉を補償で閉じるしかない — その往復が台帳に
	// 1 行も残らないと、次のティックで全ゲートが通ってまた建てる(実弾で何往復もする)。
	// SL が帯の外に出る建玉は**建てる前に**見送る(TP が帯の外なのは機会損失なので
	// 建てる。TP 脚は ExecuteOrder が落とし、OnTick が引き継ぐ)。
	if d := risk.EvaluatePriceLimit(sig, priceLimitRef(in)); !d.Allowed {
		res.RejectReason = d.Reason
		c.recordRejection(ctx, sig, res.RejectReason, in.Now)
		return res, nil
	}

	// 🛑 制度信用の**売建は貸借銘柄でしか出せない**。broker に拒否させると
	// 「拒否は標本の穴として静かに残る」ので、発注前に落として理由を台帳へ書く。
	if d := risk.EvaluateShortLoanable(sig, c.Loanable); !d.Allowed {
		res.RejectReason = d.Reason
		c.recordRejection(ctx, sig, res.RejectReason, in.Now)
		return res, nil
	}

	// Fat-finger backstop, AFTER sizing so the check sees the real quantity.
	if d := risk.EvaluateOrderBoundary(sig, qty, c.Boundary); !d.Allowed {
		res.RejectReason = d.Reason
		c.recordRejection(ctx, sig, res.RejectReason, in.Now)
		return res, nil
	}

	// 🚨 **期日が出せないなら多日建玉は建てない**。休場カレンダーが
	// 尽きて 9 営業日先が引けないとき、従来は当日限りの守りで建てていた = 翌日から裸。
	// しかも Arm / Reprice / Replace は同じ状態で error を返すので誰も直せない。
	// 建玉が無ければ守るものも無い。intraday は 14:50 に強制フラット化されるので建てる。
	ocoExpireOn := settleOrderExpiry(c.hours, sig.HoldingMode, in.Now)
	if sig.HoldingMode == order.HoldingMultiday && c.hours.TZ != nil && ocoExpireOn.IsZero() {
		res.RejectReason = "protective_expiry_unavailable_calendar_exhausted"
		c.recordRejection(ctx, sig, res.RejectReason, in.Now)
		return res, nil
	}

	// 🛑 Never-overridable brakes are re-checked as the very last step before the
	// order leaves. EvaluateStructural
	// already ran them, but this is the one place every entry path — including any
	// future manual override — must pass, so the check lives here, outside the
	// reorderable gate table.
	if d := risk.EvaluateHardSafety(sig, in.Config, snap, in.Summary); !d.Allowed {
		res.RejectReason = d.Reason
		c.recordRejection(ctx, sig, res.RejectReason, in.Now)
		return res, nil
	}

	posID, err := c.executor.Execute(ctx, ExecuteOrderInput{
		Signal:      sig,
		Quantity:    qty,
		ExecKind:    execKind,
		Source:      position.SourceBot,
		OCOExpireOn: ocoExpireOn,
		// 約定値から作る TP が帯の外なら executor が TP 脚を落とす(SL は必ず出す)。
		PriceLimitRef: priceLimitRef(in),
	})
	if err != nil {
		// 🛑 サーガの失敗も見送りとして残す。ここが素通しだと、実弾で
		// 何往復もした日の台帳が「何もしなかった日」に見える。
		res.RejectReason = "entry_execution_failed"
		c.recordRejection(ctx, sig, res.RejectReason, in.Now)
		return res, err
	}
	res.Entered = true
	res.PositionID = posID
	// エントリー成功は状態変化なので、次の同種 reject は新しい事象として記録する。
	// 🛑 **見送り側(lastNoTrade)は消さない**。あれは「その日そのアームで床に落ちたか」
	// の 1 ビットで、建玉が入るたびに解除すると、床の flap がまた毎ティック書かれる。
	delete(c.lastReject, rejectKey{symbol: sig.Symbol, strategy: sig.StrategyName})
	return res, nil
}

// recordRejection is best-effort: observability must never abort the cycle, so
// the insert error is dropped.
func (c *TradingCycle) recordRejection(ctx context.Context, sig strategy.Signal, reason string, now time.Time) {
	if c.lastReject == nil {
		c.lastReject = map[rejectKey]rejectEdge{}
	}
	c.recordOn(ctx, c.lastReject, sig, reason, now)
}

// recordNoTrade は **entry 提案が出る前に**我々が断った見送り
// (`recordableNoTradeReasons`)を、entry 拒否とは**別のエッジ表**で記録する。
// 同じ表を共有すると理由の交互出現で dedup がすり抜ける(lastNoTrade の注記)。
func (c *TradingCycle) recordNoTrade(ctx context.Context, sig strategy.Signal, reason string, now time.Time) {
	if c.lastNoTrade == nil {
		c.lastNoTrade = map[rejectKey]rejectEdge{}
	}
	c.recordOn(ctx, c.lastNoTrade, sig, reason, now)
}

func (c *TradingCycle) recordOn(ctx context.Context, seen map[rejectKey]rejectEdge, sig strategy.Signal, reason string, now time.Time) {
	if c.Rejections == nil {
		return
	}
	kind, detail := splitRejectReason(reason)
	tz := c.hours.TZ
	if tz == nil {
		tz = time.UTC
	}
	edge := rejectEdge{kind: kind, configID: sig.ConfigID, day: now.In(tz).Format("2006-01-02")}
	key := rejectKey{symbol: sig.Symbol, strategy: sig.StrategyName}
	if seen[key] == edge {
		return // 前回と同じ状態 — エッジではない
	}
	// insert 成功時だけエッジを進める — 一過性の DB 障害で「そのエピソード唯一の
	// 1行」を永久に落とさない(失敗すれば次の tick がリトライになる)。
	if err := c.Rejections.InsertRejection(ctx, port.SignalRejection{
		Symbol: sig.Symbol, ConfigID: sig.ConfigID, Reason: kind, Detail: detail, CreatedAt: now,
	}); err == nil {
		seen[key] = edge
	}
}

// splitRejectReason relies on gate.go emitting every reason as "kind [可変部]".
// Storing the stable kind separately is what makes signal_rejections GROUP-BY-able.
func splitRejectReason(reason string) (kind, detail string) {
	if i := strings.IndexByte(reason, ' '); i >= 0 {
		return reason[:i], reason[i+1:]
	}
	return reason, ""
}

// applyQtyMultiplier floors at the broker minimum lot so the consecutive-loss
// halving never produces an invalid order.
// priceLimitRef は値幅制限の基準値段 = 前日終値。運用側の日足は fetch-daily が
// ChainLinkSplits を通して保存しているので、最終バーの終値をそのまま使える。
// 日足が無い構成(backtest 等)は 0 = 判定しない。
func priceLimitRef(in strategy.EvalInput) float64 {
	if n := len(in.CandlesDaily); n > 0 {
		return in.CandlesDaily[n-1].Close
	}
	return 0
}

func applyQtyMultiplier(qty int, mul float64, minQty int) int {
	if mul <= 0 {
		mul = 1.0
	}
	scaled := int(float64(qty) * mul)
	if minQty > 0 && scaled < minQty {
		return minQty
	}
	if scaled < 1 {
		return 1
	}
	return scaled
}

// settleMaxTradingDays は broker 側の守りを何営業日先まで残すか。
//
// 立花の sOrderExpireDay は「0:当日 / それ以外は YYYYMMDD(**10営業日迄**)」。
// 上限ちょうどを狙わないのは、仕様の「指定した日を含む」が含む/含まないの
// どちらとも読めるため — 1 日余らせて拒否のリスクを避ける。
const settleMaxTradingDays = 9

// settleOrderExpiry は守りの注文期日を決める。zero = 当日限り。
//
// 🛑 multiday で zero を返してはいけない。当日期限の守りは**建てた日の引けで
// 消える**ので、2日目以降の建玉が裸になる。逆に intraday は 14:50 に
// 強制フラット化されるので当日限りが正しい — 建玉が無いのに決済注文だけ翌日の板に
// 残る方が危ない。
//
// 休場カレンダーが尽きたら zero に倒す(NthTradingDayFrom の fail-close)。
// 日付を捏造して休場日を指定すると注文ごと拒否される = 守りゼロで建つ。
// 🛑 multiday で zero を受けた TradingCycle は**建てない**。当日限りで
// 建てると翌日から裸で、復旧経路(Arm / Reprice / Replace)も同じ理由で error になる。
func settleOrderExpiry(hours session.TradingHours, mode order.HoldingMode, now time.Time) time.Time {
	if mode != order.HoldingMultiday || hours.TZ == nil {
		return time.Time{}
	}
	return hours.NthTradingDayFrom(now, settleMaxTradingDays)
}
