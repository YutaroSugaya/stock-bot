package command

import (
	"context"
	"sync"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// Within grace a drift is deferred (broker not yet caught up); past grace the
// position is cold-closed; the hard window is only reachable when the cold close
// cannot value the position, and it trips.
const (
	defaultStaleGracePeriod    = 90 * time.Second
	defaultStaleHardTripPeriod = 10 * time.Minute
	// 決済注文が板に残っているように見える状態を許す上限。守りは cancel 済みなので、
	// ここを無限にすると「裸のまま静かに滞留する」状態を作る。1 取引日を超えたら
	// 人間を呼ぶ(当日限りの注文なら翌日には消えているはずで、消えていないなら
	// それは自分の注文ではない公算が高い)。
	defaultCloseRestingLimit = 8 * time.Hour
)

// Reconcile reconciles the broker book against the DB. It never fabricates PnL:
// a cold close books the best OBSERVABLE price, and an unresolvable drift is
// deferred or tripped, never guessed at zero.
type Reconcile struct {
	broker      port.Broker
	posRepo     port.PositionRepository
	closer      port.PositionCloser
	pending     port.PendingPositionTracker
	emergency   EmergencyController
	clock       clock.Clock
	gracePeriod time.Duration
	hardPeriod  time.Duration
	// restingLimit は「決済注文が板に残っているように見える」状態を許す上限。
	restingLimit time.Duration
	// carry は信用建玉の資金コスト(買方金利 / 貸株料)。ゼロ値なら carry は 0 —
	// 料率が config に入っていない構成でレートを捏造しないため(closeOne と同じ規約)。
	carry position.CarryCalc
	Ops   OpsCounters // optional, nil-safe (observability only)

	mu           sync.Mutex
	staleSince   map[string]time.Time // brokerPositionID -> first seen stale
	closingSince map[int64]time.Time  // positionID -> first seen stuck CLOSING
}

// NewReconcile wires the reconciler. closer records cold closes; it must not be
// nil in production wiring.
func NewReconcile(b port.Broker, pr port.PositionRepository, closer port.PositionCloser, pending port.PendingPositionTracker, em EmergencyController, c clock.Clock) *Reconcile {
	if c == nil {
		c = clock.System()
	}
	return &Reconcile{
		broker: b, posRepo: pr, closer: closer, pending: pending, emergency: em, clock: c,
		gracePeriod: defaultStaleGracePeriod, hardPeriod: defaultStaleHardTripPeriod,
		restingLimit: defaultCloseRestingLimit,
		staleSince:   make(map[string]time.Time), closingSince: make(map[int64]time.Time),
	}
}

// WithCarry は信用建玉の資金コストの計算器を挿す。**挿さない構成では carry 0**
// (料率を捏造しない)。production の wiring は closeOne / ForceFlatten と同じものを渡す。
func (r *Reconcile) WithCarry(c position.CarryCalc) *Reconcile {
	r.carry = c
	return r
}

type ReconcileReport struct {
	Adopted    int
	Deferred   int
	Tripped    int
	ColdClosed int // stale/stuck positions resolved by booking a cold close
	// StuckClosing は「決済を出したが約定が確認できず CLOSING のまま」の件数。
	// この間、守りの脚は決済前に cancel 済みで**建玉は裸**なので、黙って積もらせない
	// ための口(呼び手が log/監視に出す)。escalate() には入らない — 板に決済注文が
	// 残っている限り trip はしない設計だが、見えないのは別問題。
	StuckClosing int
	// ExternalUnbooked は「external 建玉が broker から消えたが、台帳に決済を書けなかった」
	// 件数(価格や建値が観測できない / DB エラー)。**口座には実額として効いたのに台帳に
	// 残らなかった**ということなので、黙って落とさず呼び手が log に出す。
	ExternalUnbooked int
}

func (r *Reconcile) Run(ctx context.Context, symbol string) (ReconcileReport, error) {
	now := r.clock()
	var rep ReconcileReport

	brokerPositions, err := r.broker.GetPositions(ctx)
	if err != nil {
		return rep, err
	}
	dbPositions, err := r.posRepo.ListOpenOrClosing(ctx, symbol)
	if err != nil {
		return rep, err
	}

	brokerByID := make(map[string]port.BrokerPosition)
	for _, bp := range brokerPositions {
		if bp.Symbol == symbol {
			brokerByID[bp.BrokerPositionID] = bp
		}
	}
	dbByBrokerID := make(map[string]position.Position)
	for _, p := range dbPositions {
		if p.BrokerPositionID != "" {
			dbByBrokerID[p.BrokerPositionID] = p
		}
	}

	for id, bp := range brokerByID {
		if _, known := dbByBrokerID[id]; known {
			continue
		}
		if r.pending != nil && (r.pending.IsPending(id) || r.pending.IsPendingSymbol(bp.Symbol)) {
			continue // entry saga in flight; do not mis-adopt
		}
		if _, err := r.posRepo.AdoptExternal(ctx, bp, now); err != nil {
			return rep, err
		}
		rep.Adopted++
		if r.Ops != nil {
			r.Ops.IncrExternalAdoptions()
		}
		// No working close-side order = no broker-side 守り (e.g. the bot crashed
		// between fill and OCO placement). Never let that orphan ride unnoticed.
		if !r.hasWorkingCloseOrder(ctx, bp.Symbol, bp.Side.Opposite()) {
			if r.emergency != nil {
				_ = r.emergency.Trip("external_adopt_unprotected:"+id, now)
			}
			rep.Tripped++
		}
	}

	for id, p := range dbByBrokerID {
		if _, stillThere := brokerByID[id]; stillThere {
			r.clearStale(id)
			// Stuck CLOSING (close rejected / lost): no other path revisits a CLOSING
			// row, so it is retried here or it is orphaned forever.
			if p.Status == position.StatusClosing && p.Source != position.SourceExternal {
				r.resolveStuckClosing(ctx, p, now, &rep)
			} else {
				r.clearClosing(p.ID)
			}
			continue
		}
		if p.Source == position.SourceExternal {
			// Its owner closed it. **台帳には残す**: その PnL は bot の
			// 戦略成績ではないが、**口座には実額として効いている**(委託保証金・維持率・
			// 日次損失)。消すと監査で復元できない。戦略の出口ではないので
			// close_reason='external_close' で区別し、エッジ判定側で外す。
			// 価格が観測できなければ trade は書かない(PnL 捏造禁止)が、建玉は必ず
			// 閉じる — OPEN のまま残すとその銘柄のナンピン禁止が永久に閉じる。
			if price := r.observablePrice(ctx, p.Symbol); price > 0 {
				if r.bookCloseAs(ctx, p, price, now, port.CloseReasonExternalClose) {
					continue
				}
			}
			// 記帳できなかった(価格・建値が観測できない / DB エラー)。建玉は閉じるが、
			// **黙って落とさない** — 実額が台帳から消えた事実を報告に載せる。
			rep.ExternalUnbooked++
			_ = r.posRepo.MarkClosed(ctx, p.ID, now)
			continue
		}
		first := r.markStale(id, now)
		age := now.Sub(first)
		// 🛑 **先に解決を試み、解決できなかったときだけ trip する。**
		// 経過時間で分岐を選ぶ書き方(hardPeriod を先に見る)だと、ポーリング間隔が
		// 猶予窓より長い構成で cold close の枝に**到達できない**。live の reconcile は
		// 口座照会を絞るため建玉保有中でも1時間おきなので、2回目の観測で age が
		// 1時間になり 90秒〜10分の窓を飛び越えていた = broker 側で TP/SL が約定する
		// たびに決済が台帳に残らないまま緊急停止していた。
		// trip は「未解決」に対する手段であって、時間の経過そのものに対する手段ではない。
		if age < r.gracePeriod {
			rep.Deferred++
			continue
		}
		// The protective OCO filled or it was closed outside the bot: book the
		// close so the position and the daily-loss ledger converge with reality.
		if r.coldClose(ctx, p, now) {
			r.clearStale(id)
			r.clearClosing(p.ID)
			rep.ColdClosed++
			continue
		}
		// 解決できなかった(価格が観測できない等)。長く未解決なら人間を呼ぶ。
		if age >= r.hardPeriod {
			if r.emergency != nil {
				_ = r.emergency.Trip("reconcile_stale_position_unresolved:"+id, now)
			}
			rep.Tripped++
			continue
		}
		rep.Deferred++
	}
	return rep, nil
}

// resolveStuckClosing waits within grace (the original close may still fill),
// re-issues a MARKET close past grace when nothing is working at the broker, and
// trips past the hard window — a close stuck that long needs a human.
func (r *Reconcile) resolveStuckClosing(ctx context.Context, p position.Position, now time.Time, rep *ReconcileReport) {
	first := r.markClosing(p.ID, now)
	age := now.Sub(first)
	// 🛑 上の stale 判定と同じ理由で **先に解決を試みる**。hardPeriod を先に見ると、
	// ポーリング間隔が猶予窓より長い構成(live は 1 時間)で再送に到達できず、
	// 「座礁した決済を人間に投げる」だけの経路になる。
	escalate := func() {
		if age >= r.hardPeriod {
			if r.emergency != nil {
				_ = r.emergency.Trip("close_stuck_unresolved:"+p.BrokerPositionID, now)
			}
			rep.Tripped++
			return
		}
		rep.Deferred++
	}
	switch {
	case age >= r.gracePeriod:
		if r.hasRestingSettleOrder(ctx, p) {
			// 🛑 **待ちには上限を置く。** 板に決済側の注文が見えているだけでは
			// 「自分の再送がまだ生きている」証明にならない(注文 id を持たないので
			// 人間の注文と区別できない)。守りは既に cancel 済みなので、ここで
			// 無限に defer すると **裸のまま静かに滞留する**状態を作る。
			// 1 取引日を超えたら別理由で人間を呼ぶ。
			if age >= r.restingLimit {
				if r.emergency != nil {
					_ = r.emergency.Trip("close_resting_too_long:"+p.BrokerPositionID, now)
				}
				rep.Tripped++
				return
			}
			rep.Deferred++ // close order still working at the broker; let it fill
			rep.StuckClosing++
			return
		}
		res, err := r.broker.ClosePosition(ctx, port.CloseRequest{
			Symbol: p.Symbol, BrokerPositionID: p.BrokerPositionID,
			Side: p.Side.Opposite(), Quantity: p.Quantity, ExecKind: p.ExecKind,
		})
		if err != nil || res == nil || !res.Accepted {
			escalate() // 再送も通らない — 長く未解決なら人間を呼ぶ
			return
		}
		// 🚨 受理は約定ではない。ここで観測価格を埋めて CLOSED を書くと、closeOne で
		// 塞いだ幽霊決済がそのまま復活する — しかも未約定の決済は当日限りで消えるので
		// **翌営業日の再送は必ずこの枝に来る**(穴が 1 日ずれるだけだった)。
		price, _, confirmed := resolveSettleFill(ctx, r.broker, res, p.Quantity)
		if !confirmed {
			// 再送した成行は板に残す(比例配分への唯一の参加経路)。次のラウンドで
			// hasRestingSettleOrder が true になり、約定すれば cold close が拾う。
			//
			// 🚨 ただし **板に何も無ければ話が別**。守りは cancel 済みなので、
			// 再送も載っていないならこの建玉は逆指値も決済注文も持たない = 裸。
			// closeOne が 763782c で塞いだ穴が、1 時間後の再送枝にそのまま残っていた。
			tripIfSettleLeavesPositionNaked(ctx, r.broker, r.emergency,
				p.Symbol, p.Side.Opposite(), p.BrokerPositionID, res, now)
			rep.Deferred++
			rep.StuckClosing++
			return
		}
		if price <= 0 {
			price = r.observablePrice(ctx, p.Symbol)
		}
		if price <= 0 {
			rep.Deferred++ // close accepted; the stale branch books it once the broker drops it
			return
		}
		if r.bookClose(ctx, p, price, now) {
			r.clearClosing(p.ID)
			rep.ColdClosed++
		} else {
			rep.Deferred++
		}
	default:
		rep.Deferred++
	}
}

// coldClose returns false when the position cannot be valued honestly (no
// price); the caller keeps deferring and the hard window eventually trips.
func (r *Reconcile) coldClose(ctx context.Context, p position.Position, now time.Time) bool {
	// 🛑 **まず実約定を探す。**建玉が broker から消える理由のほとんどは
	// **板の守り(逆指値)が約定した**こと = 戦略の出口そのもので、値段も手数料も
	// 約定照会に載っている。時価で埋めると台帳は「照合で消失」という**起きていない
	// 出口**を、観測時刻の値段で記録する(8604: 実約定 1,596 の損切りが
	// 92 分後の 1,596.5 で `reconcile_cold_close` になっていた)。
	if price, fee, reason, ok := boardSettleFill(ctx, r.broker, p); ok {
		return r.book(ctx, p, price, fee, false, now, reason)
	}
	price := r.observablePrice(ctx, p.Symbol)
	if price <= 0 {
		return false // never book a 0-price trade (PnL 捏造禁止)
	}
	return r.bookClose(ctx, p, price, now)
}

func (r *Reconcile) bookClose(ctx context.Context, p position.Position, price float64, now time.Time) bool {
	return r.bookCloseAs(ctx, p, price, now, "reconcile_cold_close")
}

// bookCloseAs は **決済脚の手数料が同期的に取れない**経路の記帳。凍結した entry
// 手数料だけを載せ、行を estimated にする。実額が取れた経路は book を直に呼ぶ。
func (r *Reconcile) bookCloseAs(ctx context.Context, p position.Position, price float64, now time.Time, reason string) bool {
	return r.book(ctx, p, price, p.EntryFeeJPY, true, now, reason)
}

func (r *Reconcile) book(ctx context.Context, p position.Position, price, fee float64,
	feeEstimated bool, now time.Time, reason string) bool {
	if r.closer == nil {
		return false
	}
	// 🚨 建値ゼロの建玉で PnL を作らない。external の adopt は broker の申告値をそのまま
	// 凍結するので、建値が取れなかった建玉は EntryPrice=0 で入る。そのまま決済すると
	// grossPnL が (現値 − 0) × 数量 = **現値ぶん丸ごとを利益**として書き、日次損失 cap と
	// 維持率の判断材料が嘘になる(例: 100株 @2,600 で +260,000 円の偽益)。
	// 価格ゼロを弾く coldClose と同じ極性 — 確かめられない値は書かない。
	if p.EntryPrice <= 0 || p.Quantity <= 0 {
		return false
	}
	if p.Status == position.StatusOpen {
		ok, err := r.posRepo.ClaimForClose(ctx, p.ID, now)
		if err != nil || !ok {
			return false
		}
	}
	ok, err := r.closer.CloseAndRecord(ctx, p.ID, now, port.TradeRecord{
		PositionID: p.ID, Symbol: p.Symbol, Side: p.Side, Quantity: p.Quantity,
		EntryPrice: p.EntryPrice, ClosePrice: price, ProfitLossJPY: grossPnL(p, price),
		FeeJPY: fee, FeeEstimated: feeEstimated,
		// 🛑 carry(信用建玉の資金コスト)は **closeOne と同じ規約**で載せる。
		// ここだけ 0 にしていたため、10 営業日持った建玉が broker 側の守りで
		// 決済されると net = gross − fee と読めてしまい、同じ出口が経路によって
		// 別の net を持っていた。料率が無い構成では CarryCalc がゼロ値 = carry 0。
		CarryJPY:    r.carry.JPY(p, now),
		CloseReason: reason, ClosedAt: now,
	})
	return err == nil && ok
}

// hasWorkingCloseOrder detects a broker-side protective exit. Fail-safe: a
// listing error reports false.
//
// 🛑 **逆指値脚(`HasStopLeg`)を要求する**。決済側に注文があることは守りではない —
// 利確指値だけが板にある建玉は下方向に裸で、SL はどこにも存在しない。ここが side
// だけを見ていたため、external_adopt_unprotected は「売り注文が 1 本でもあれば
// 守られている」と読んでいた。成行の決済注文が
// 在庫中の建玉もここでは守り無しに数えるが、それは trip する側 = 安全側に倒れる。
// hasRestingSettleOrder は「**自分が出した返済注文がまだ板にあるか**」を見る。
//
// 🛑 hasWorkingCloseOrder と**別の述語**である。取り違えると両方向に壊れる:
//   - あちらは `HasStopLeg` を要求する。問いが「守りがあるか」だから — 利確指値だけが
//     板にある建玉は下方向に裸(実口座で実測)。
//   - こちらの問いは「再送した**成行**が板に載ったか」。成行の返済注文は逆指値脚を
//     持たないので、HasStopLeg で絞ると自分の注文を毎回「無い」と読み、
//     ラウンドごとに再送し続ける。live は 1 時間間隔で hardPeriod(10 分)を必ず
//     超えるので、2 巡目で拘束による拒否 → close_stuck_unresolved を trip する。
//     それは exit_executor.go が「trip してはいけない」と明記した
//     **ストップ安の比例配分待ち**そのもの。
//
// 🚨 数量を `>= p.Quantity` で絞る。決済注文 id を持たないので「誰の注文か」は
// 区別できず、人間の端数注文を「自分の全量が板にある」と読むと**守りが無いまま
// 永久に defer する**状態を作る(守りは cancel 済み = 裸)。板に人間の決済注文が
// 載っているのは空想ではない — 実口座で 4704 は 100株の建玉に
// 決済注文が 200株 載っていた。`protectiveOrderIsResting` と同じ ≥ qty 規則。
func (r *Reconcile) hasRestingSettleOrder(ctx context.Context, p position.Position) bool {
	orders, err := r.broker.GetActiveOrders(ctx, p.Symbol)
	if err != nil {
		return false // 照会できない = 載っている証明が無い(fail-close)
	}
	closeSide := p.Side.Opposite()
	for _, o := range orders {
		if o.Side == closeSide && o.Quantity >= p.Quantity {
			return true
		}
	}
	return false
}

func (r *Reconcile) hasWorkingCloseOrder(ctx context.Context, symbol string, closeSide order.Side) bool {
	orders, err := r.broker.GetActiveOrders(ctx, symbol)
	if err != nil {
		return false
	}
	for _, o := range orders {
		if o.Side == closeSide && o.HasStopLeg {
			return true
		}
	}
	return false
}

func (r *Reconcile) observablePrice(ctx context.Context, symbol string) float64 {
	t, err := r.broker.GetTicker(ctx, symbol)
	if err != nil || t == nil {
		return 0
	}
	return t.Mid()
}

func (r *Reconcile) markStale(id string, now time.Time) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.staleSince[id]; ok {
		return t
	}
	r.staleSince[id] = now
	return now
}

func (r *Reconcile) clearStale(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.staleSince, id)
}

func (r *Reconcile) markClosing(id int64, now time.Time) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.closingSince[id]; ok {
		return t
	}
	r.closingSince[id] = now
	return now
}

func (r *Reconcile) clearClosing(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.closingSince, id)
}
