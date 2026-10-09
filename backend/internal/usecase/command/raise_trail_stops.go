package command

import (
	"context"
	"fmt"
	"sort"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
)

// RaiseTrailStopsResult は 1 周ぶんの結果。
type RaiseTrailStopsResult struct {
	Raised int // 板の SL を線まで引き上げた本数
}

// RaiseTrailStops は **寄り前に、線が立っている trail 建玉の板の SL を利確の線まで引き上げる**
//
// 🚨 なぜ要るか: trail の利確の線(max(arm, peak − giveback)・+1×ATR 以上)は bot の OnTick だけが
// 持ち、板に載っている守りは −2×ATR の SL だけ。bot が場中に居ないと、線を割っても誰も売らず
// 板の SL まで落ちる(差は 3×ATR)。起動の失敗は実際に何度も起きている。
//
// 🛑 縛り:
//   - **寄り前だけ**(場中は撃たない)。取消と再発注の間は守りが消える。場中に上がった線は翌朝まで
//     板に載らない(場中は bot が持つ)。
//   - **上げるだけ**。板の SL が既に線以上なら触らない。
//   - **前日終値が線以下なら置かない**。線を割ったのに bot が売っていない状態で、売りの逆指値を
//     現値より上に置いたときの立花の挙動は未実測。名指しで返す(寄りの決済は bot に任せる)。
//   - 取消 → 再発注・呼値・帯の検問・期日・台帳の SL の更新・失敗時の trip は
//     RepriceProtectiveOrder に任せる(同じ検問を 2 通り持たない)。
//   - 期日の置き直し(ReplaceProtectiveOrder)の**後に同じ流れで**回す(cmd の配線)。別 goroutine に
//     すると同じ銘柄で取消 → 再発注が重なる。
type RaiseTrailStops struct {
	positions port.PositionRepository
	board     port.ProtectiveOrderBoard
	hours     session.TradingHours
	clock     clock.Clock
	reprice   *RepriceProtectiveOrder
	// refPrice は前日終値。nil / 0 は「判定できない」= 前日終値の検査をしない。
	refPrice func(ctx context.Context, symbol string) float64
}

func NewRaiseTrailStops(pr port.PositionRepository, board port.ProtectiveOrderBoard,
	hours session.TradingHours, clk clock.Clock, reprice *RepriceProtectiveOrder) *RaiseTrailStops {
	return &RaiseTrailStops{positions: pr, board: board, hours: hours, clock: clk, reprice: reprice}
}

// WithPriceLimitRef は前日終値の引き口を挿す(RepriceProtectiveOrder と同じもの)。
func (r *RaiseTrailStops) WithPriceLimitRef(fn func(ctx context.Context, symbol string) float64) *RaiseTrailStops {
	r.refPrice = fn
	return r
}

// Execute は 1 周ぶん。
func (r *RaiseTrailStops) Execute(ctx context.Context) (RaiseTrailStopsResult, []error) {
	var res RaiseTrailStopsResult
	if r.hours.InTradingHours(r.clock()) {
		return res, nil
	}
	guarded, err := multidayGuardedPositions(ctx, r.positions)
	if err != nil {
		return res, []error{fmt.Errorf("板の SL の引き上げ: 建玉を読めない: %w", err)}
	}
	syms := make([]string, 0, len(guarded))
	for sym := range guarded {
		syms = append(syms, sym)
	}
	sort.Strings(syms)

	var errs []error
	for _, sym := range syms {
		// 値段の変更は建玉を 1 つに確定できる銘柄だけ(RepriceProtectiveOrder と同じ条件)。
		if len(guarded[sym]) != 1 {
			continue
		}
		pos := guarded[sym][0]
		line, ok := position.TrailFloorStopPrice(pos)
		if !ok {
			continue
		}
		raised, err := r.raiseOne(ctx, pos, line)
		if err != nil {
			errs = append(errs, err)
		}
		if raised {
			res.Raised++
		}
	}
	return res, errs
}

func (r *RaiseTrailStops) raiseOne(ctx context.Context, pos position.Position, line float64) (bool, error) {
	sym := pos.Symbol
	orders, err := r.board.ListProtectiveOrders(ctx, sym)
	if err != nil {
		return false, fmt.Errorf("板の SL の引き上げ %s: 注文照会に失敗 — 触らない: %w", sym, err)
	}
	var current float64
	for _, o := range orders {
		if o.HasStopLeg && o.Side == pos.Side.Opposite() {
			current = o.StopTrigger
			break
		}
	}
	// 板に守りが無いのは消えた守りの復旧(RearmUnguarded)の管轄。ここで新規に置かない。
	if current <= 0 {
		return false, nil
	}
	if !stopIsTighter(pos.Side, line, current) {
		return false, nil
	}
	if r.refPrice != nil {
		if ref := r.refPrice(ctx, sym); ref > 0 && !stopIsTighter(pos.Side, ref, line) {
			return false, fmt.Errorf("板の SL の引き上げ %s: 前日終値 %g が利確の線 %g を既に割っている — "+
				"**置かない**(現値より上の売りの逆指値の挙動は未実測)。bot が止まっていて線で売れなかった可能性。"+
				"寄りの決済は bot の OnTick に任せる(板の SL は %g のまま)", sym, ref, line, current)
		}
	}
	if _, err := r.reprice.Execute(ctx, RepriceProtectiveInput{Symbol: sym, StopLoss: line}); err != nil {
		return false, fmt.Errorf("板の SL の引き上げ %s(%g → %g): %w", sym, current, line, err)
	}
	return true, nil
}

// stopIsTighter は a が b より建玉に有利な側(買いなら上・売りなら下)にあるか。
func stopIsTighter(side order.Side, a, b float64) bool {
	if side == order.SideSell {
		return a < b
	}
	return a > b
}
