package protectiveboard

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// State は **broker の板にいま載っている守り**の写し(read-only)。
//
// 🚨 なぜ要るか:
// 画面の TP/SL は `list_open_positions` の **台帳の凍結値**を出していて、DTO には
// 「broker の OCO に入っているのと同じ値段」と書いてあった。**それが嘘になる経路が
// 2 つある**:
//
//  1. 人間が証券アプリで TP/SL を締める。凍結値は
//     建てたときの値なので、画面は**実際には起こらない値段**を出し続ける。
//  2. 守りが板から消える(期日切れ / 取消 → 再発注の失敗)。live で守りが消えていた 46 分間、
//     画面は凍結値の TP/SL を平常どおり出していて、**裸であることがどこにも
//     出ていなかった**。
//
// 🛑 **凍結値を消さない。**凍結値は「戦略が決めた出口」で、板の値は「実際に起きる
// こと」。どちらも意味がある。画面は板を主にして、食い違ったときだけ凍結値を添える。
type State struct {
	mu        sync.Mutex
	fetchedAt time.Time
	guards    map[string]port.ProtectiveOrderInfo
	unguarded []string
	// closingNaked は **決済中で、守りも決済注文も板に無い**銘柄。unguarded と
	// 分けるのは意味が違うから — CLOSING は「決済が進行中かもしれない」状態で、
	// 復旧の手順(arm)は同じでも読み方が違う。
	// 🚨 CLOSING をスイープから丸ごと除外していると、
	// 建玉が裸で CLOSING に落ちたとき、画面にもログにも何も出ない。
	closingNaked []string
	// unknown は **照会そのものが失敗した**銘柄。どのリストにも入れずに落とすと、
	// 障害中は「守りが本当に消えていても画面が完全に無言」になる(fail-open)。
	// 分からないことを分からないと出す。
	unknown   []string
	lastErr   string
	lastErrAt time.Time
}

// set は照会が成功した回の写し。guards は銘柄 → 板の守り、unguarded は
// **多日建玉なのに板に守りが無い**銘柄(= 裸)。
func (s *State) set(now time.Time, guards map[string]port.ProtectiveOrderInfo,
	unguarded, closingNaked, unknown []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetchedAt = now
	s.guards = guards
	s.unguarded = unguarded
	s.closingNaked = closingNaked
	s.unknown = unknown
	if err != nil {
		s.lastErr, s.lastErrAt = err.Error(), now
	}
}

// setErr は照会そのものが落ちた回。
//
// 🛑 **直前の写しを消さない。**消すと「照会できなかった」と「守りが無い」が
// 区別できない空の画面になり、障害が裸の警報に化ける(またはその逆)。
// fetchedAt も動かさない —— 画面に出ている値がいつ時点のものかがずれる。
func (s *State) setErr(now time.Time, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr, s.lastErrAt = msg, now
}

func (s *State) View() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(clock.JST).Format("2006-01-02 15:04:05")
	}
	// 🛑 **一度も読んでいない**を「守りが無い」と読ませない。起動直後や照会失敗で
	// 空のまま unguarded を出すと、毎朝ぶんの偽の裸警報が出て麻痺する。
	if s.fetchedAt.IsZero() {
		return map[string]any{
			"fetched":    false,
			"error":      s.lastErr,
			"error_at":   stamp(s.lastErrAt),
			"note":       "板の守りをまだ一度も読めていない。**画面の TP/SL は台帳の凍結値**で、人間がアプリで締めた値は反映されていない",
			"by_symbol":  map[string]any{},
			"expires_on": "",
		}
	}
	by := map[string]any{}
	for sym, g := range s.guards {
		exp := ""
		if !g.ExpireOn.IsZero() {
			exp = g.ExpireOn.In(clock.JST).Format("2006-01-02")
		}
		by[sym] = map[string]any{
			"order_id":    g.OrderID,
			"take_profit": g.LimitPrice,
			"stop_loss":   g.StopTrigger,
			"quantity":    g.Quantity,
			"expire_on":   exp,
		}
	}
	return map[string]any{
		"fetched":    true,
		"fetched_at": stamp(s.fetchedAt),
		"by_symbol":  by,
		"unguarded":  s.unguarded,
		// 🚨 決済中で守りも決済注文も板に無い = 裸。unguarded と別の籠に出す。
		"closing_naked": s.closingNaked,
		// 🛑 照会できなかった銘柄。空リストと「守りがある」を混同させない。
		"unknown":  s.unknown,
		"error":    s.lastErr,
		"error_at": stamp(s.lastErrAt),
		"note":     "**板にいま載っている守り**。台帳の凍結値ではない(人間がアプリで締めた値もここに出る)",
	}
}

// Refresh は多日建玉の銘柄ぶんだけ板を読み直す。
//
// 🛑 **建玉の数しか呼ばない。**立花のレート予算は日次で有限なので、universe 全体を
// 舐めない。多日の bot 建玉が無ければ 1 回も呼ばない。
func Refresh(ctx context.Context, positions port.PositionRepository,
	brk interface {
		ListProtectiveOrders(ctx context.Context, symbol string) ([]port.ProtectiveOrderInfo, error)
	}, st *State, now time.Time, logger *slog.Logger) {

	held, err := positions.ListOpenAllSymbols(ctx)
	if err != nil {
		st.setErr(now, "建玉一覧が読めない: "+err.Error())
		return
	}
	bySym := map[string][]position.Position{}
	for _, p := range held {
		// 🚨 **CLOSING を落とさない**。守りを cancel した後に決済が板へ
		// 載らないと建玉は CLOSING かつ裸になる。ここで除外していたせいで、
		// CLOSING で裸になった建玉は画面の 🚨 タグにもログの裸警報にも出なかった
		// —— 事故の当事者そのものが、唯一の検出器の視界の外にいた。
		if p.HoldingMode != order.HoldingMultiday || p.Source == position.SourceExternal {
			continue
		}
		bySym[p.Symbol] = append(bySym[p.Symbol], p)
	}
	syms := make([]string, 0, len(bySym))
	for sym := range bySym {
		syms = append(syms, sym)
	}
	sort.Strings(syms)

	guards := map[string]port.ProtectiveOrderInfo{}
	unguarded := []string{}
	closingNaked := []string{}
	unknown := []string{}
	for _, sym := range syms {
		orders, err := brk.ListProtectiveOrders(ctx, sym)
		if err != nil {
			// 🛑 1 銘柄の照会失敗で写し全体を捨てない(残りは読めている)。
			// 🚨 ただし **どのリストにも入れずに落とさない**。落とすと障害中は
			// 「守りが本当に消えていても画面が完全に無言」になり、
			// 裸がどこにも出ない事故の形に戻る。
			st.setErr(now, "守りの照会に失敗 "+sym+": "+err.Error())
			unknown = append(unknown, sym)
			if logger != nil {
				logger.Error("守りを確認できていない(照会失敗)", "track", "live", "symbol", sym, "err", err)
			}
			continue
		}
		found := false
		for _, o := range orders {
			// 逆指値脚を持ち、建玉と反対側 = bot の守り。利確指値だけの注文は守りではない。
			if !o.HasStopLeg || !ownsBoardGuard(bySym[sym], o) {
				continue
			}
			guards[sym] = o
			found = true
			break
		}
		if found {
			continue
		}
		// 決済中の建玉は「守りが無い」だけでは裸と断定できない —— 決済注文が板に
		// 残っていれば決済が進行中。🛑 逆指値脚の有無で絞らない(成行の返済注文は
		// 脚を持たないので、絞ると必ず取りこぼす)。
		if onlyClosing(bySym[sym]) {
			if settleResting(orders, bySym[sym]) {
				continue // 決済が板で進行中 = 正常な待ち
			}
			closingNaked = append(closingNaked, sym)
			if logger != nil {
				logger.Error("🚨 決済中の建玉に守りも決済注文も無い(裸)", "track", "live", "symbol", sym,
					"復旧", "POST /api/live/protective/arm")
			}
			continue
		}
		// 🚨 多日建玉なのに板に守りが無い = **裸**。
		unguarded = append(unguarded, sym)
		if logger != nil {
			logger.Error("🚨 多日建玉に broker 側の守りが無い(裸)", "track", "live", "symbol", sym,
				"復旧", "POST /api/live/protective/arm")
		}
	}
	st.set(now, guards, unguarded, closingNaked, unknown, nil)
}

// onlyClosing は「その銘柄の対象建玉が全部 CLOSING か」。1 本でも OPEN が混ざるなら
// 通常の裸判定に倒す(OPEN の建玉に守りが無いのは決済の進行では説明できない)。
func onlyClosing(held []position.Position) bool {
	if len(held) == 0 {
		return false
	}
	for _, p := range held {
		if p.Status != position.StatusClosing {
			return false
		}
	}
	return true
}

// settleResting は決済側の注文が板に残っているか。数量は問わない —— ここは
// 「決済が進行中に見えるか」を人間に伝えるための表示判定で、reconcile の
// 再送判定(自分の全量が載っているか)とは目的が違う。
func settleResting(orders []port.ProtectiveOrderInfo, held []position.Position) bool {
	for _, o := range orders {
		for _, p := range held {
			if o.Side == p.Side.Opposite() {
				return true
			}
		}
	}
	return false
}

func ownsBoardGuard(held []position.Position, o port.ProtectiveOrderInfo) bool {
	for _, p := range held {
		if o.Side == p.Side.Opposite() {
			return true
		}
	}
	return false
}
