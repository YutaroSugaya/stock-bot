// Package repository の in-memory 実装は pg 版と同じ不変条件(ClaimForClose の CAS / CloseAndRecord の原子性 / per-symbol 分離)を満たす。
package repository

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

type InMemoryPositionRepo struct {
	mu     sync.Mutex
	nextID int64
	byID   map[int64]*position.Position
}

func NewInMemoryPositionRepo() *InMemoryPositionRepo {
	return &InMemoryPositionRepo{byID: make(map[int64]*position.Position)}
}

func (r *InMemoryPositionRepo) Insert(_ context.Context, in port.PositionInsertInput) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	id := r.nextID
	src := in.Source
	if src == "" {
		src = position.SourceBot
	}
	var entryFee float64
	if in.EntryFeeJPY != nil {
		entryFee = *in.EntryFeeJPY
	}
	r.byID[id] = &position.Position{
		ID: id, BrokerPositionID: in.BrokerPositionID, Symbol: in.Symbol, Side: in.Side,
		Quantity: in.Quantity, EntryPrice: in.EntryPrice,
		TakeProfitJPY: in.TakeProfitJPY, StopLossJPY: in.StopLossJPY,
		TakeProfitPrice: in.TakeProfitPrice, StopLossPrice: in.StopLossPrice,
		MaxHoldMinutes:      in.MaxHoldMinutes,
		ExtensionMaxMinutes: in.ExtensionMaxMinutes, ExtensionUnrealizedJPY: in.ExtensionUnrealizedJPY,
		EarlyExitWindowMinutes: in.EarlyExitWindowMinutes, EarlyExitTargetJPY: in.EarlyExitTargetJPY,
		RatchetArmJPY: in.RatchetArmJPY, RatchetGivebackJPY: in.RatchetGivebackJPY,
		RatchetFloorAtArm: in.RatchetFloorAtArm,
		StrategyConfigID:  in.StrategyConfigID, StrategyName: in.StrategyName,
		HoldingMode: in.HoldingMode, ExecKind: in.ExecKind,
		TickSizeAtEntry: in.TickSizeAtEntry, Source: src, Status: position.StatusOpen, OpenedAt: in.OpenedAt,
		EntryFeeJPY: entryFee,
	}
	return id, nil
}

func (r *InMemoryPositionRepo) ListOpenOrClosing(_ context.Context, symbol string) ([]position.Position, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []position.Position
	for _, p := range r.byID {
		if p.Symbol == symbol && (p.Status == position.StatusOpen || p.Status == position.StatusClosing) {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *InMemoryPositionRepo) ListOpenAllSymbols(_ context.Context) ([]position.Position, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []position.Position
	for _, p := range r.byID {
		if p.Status == position.StatusOpen || p.Status == position.StatusClosing {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// 未知 id は (nil, nil)。
func (r *InMemoryPositionRepo) GetByID(_ context.Context, id int64) (*position.Position, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok {
		return nil, nil
	}
	cp := *p // コピーを返す(呼び出し側から内部状態を書き換えさせない)
	return &cp, nil
}

// external adoption も数える(委託保証金の保護対象なので除外しない)。
func (r *InMemoryPositionRepo) CountOpenAllSymbols(_ context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.byID {
		if p.Status == position.StatusOpen || p.Status == position.StatusClosing {
			n++
		}
	}
	return n, nil
}

// UpdateProtectivePrices は OPEN 建玉の台帳の TP/SL(価格と幅)を人間が変えた値へ揃える。
func (r *InMemoryPositionRepo) UpdateProtectivePrices(_ context.Context, id int64, tpPrice, slPrice, tpJPY, slJPY float64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok || p.Status != position.StatusOpen {
		return false, nil
	}
	p.TakeProfitPrice, p.StopLossPrice, p.TakeProfitJPY, p.StopLossJPY = tpPrice, slPrice, tpJPY, slJPY
	return true, nil
}

// ApplySplit は権利落ち日の分割調整を OPEN 建玉へ書く(pg 版と同じ CAS: その日にまだ調整していない行だけ)。
func (r *InMemoryPositionRepo) ApplySplit(_ context.Context, adj position.Position) (bool, error) {
	if adj.SplitAdjustedOn.IsZero() {
		return false, errors.New("ApplySplit: 権利落ち日(SplitAdjustedOn)が空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[adj.ID]
	if !ok || p.Status != position.StatusOpen {
		return false, nil
	}
	day := adj.SplitAdjustedOn.Format(time.DateOnly)
	if !p.SplitAdjustedOn.IsZero() && p.SplitAdjustedOn.Format(time.DateOnly) == day {
		return false, nil
	}
	p.Quantity, p.EntryPrice = adj.Quantity, adj.EntryPrice
	p.TakeProfitPrice, p.StopLossPrice, p.TakeProfitJPY, p.StopLossJPY = adj.TakeProfitPrice, adj.StopLossPrice, adj.TakeProfitJPY, adj.StopLossJPY
	p.ExtensionUnrealizedJPY, p.EarlyExitTargetJPY = adj.ExtensionUnrealizedJPY, adj.EarlyExitTargetJPY
	p.RatchetArmJPY, p.RatchetGivebackJPY = adj.RatchetArmJPY, adj.RatchetGivebackJPY
	p.PeakUnrealizedJPY, p.TroughUnrealizedJPY = adj.PeakUnrealizedJPY, adj.TroughUnrealizedJPY
	p.SplitFactor, p.SplitAdjustedOn = adj.SplitFactor, adj.SplitAdjustedOn
	return true, nil
}

// CountOpenedSinceBySymbolStrategy は since 以降に (銘柄, 戦略) で建てた bot 建玉の数(状態を問わない)。
func (r *InMemoryPositionRepo) CountOpenedSinceBySymbolStrategy(_ context.Context, symbol, strategy string, since time.Time) (int, error) {
	return r.countBotOpenedSince(since, func(p position.Position) bool {
		return p.Symbol == symbol && p.StrategyName == strategy
	}), nil
}

// CountOpenedSince は since 以降に口座全体で建てた bot 建玉の数(状態を問わない)。
func (r *InMemoryPositionRepo) CountOpenedSince(_ context.Context, since time.Time) (int, error) {
	return r.countBotOpenedSince(since, func(position.Position) bool { return true }), nil
}

// countBotOpenedSince は since 以降に建てた bot 建玉のうち match に合うものを数える(external は数えない)。
func (r *InMemoryPositionRepo) countBotOpenedSince(since time.Time, match func(position.Position) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.byID {
		if p.Source == position.SourceBot && !p.OpenedAt.Before(since) && match(*p) {
			n++
		}
	}
	return n
}

// CountOpenAcross は本数・銘柄数・入口の銘柄数を 1 回の走査で返す。
// external adoption も数える(監視集合に乗るので、枠の判断からは外せない)。
func (r *InMemoryPositionRepo) CountOpenAcross(_ context.Context, entryArms []string) (port.OpenCounts, error) {
	want := make(map[string]bool, len(entryArms))
	for _, a := range entryArms {
		if a != "" {
			want[a] = true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out port.OpenCounts
	syms := map[string]bool{}
	armSyms := map[string]bool{}
	for _, p := range r.byID {
		if p.Status != position.StatusOpen && p.Status != position.StatusClosing {
			continue
		}
		out.Positions++
		syms[p.Symbol] = true
		if want[p.StrategyName] {
			armSyms[p.Symbol] = true
		}
	}
	out.Symbols = len(syms)
	out.EntryArmSymbols = len(armSyms)
	return out, nil
}

// CAS: OPEN だけが CLOSING に倒せる。CLOSING/CLOSED は ok=false で二重決済を防ぐ。
func (r *InMemoryPositionRepo) ClaimForClose(_ context.Context, id int64, _ time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok || p.Status != position.StatusOpen {
		return false, nil
	}
	p.Status = position.StatusClosing
	return true, nil
}

func (r *InMemoryPositionRepo) MarkClosed(_ context.Context, id int64, closedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.byID[id]; ok {
		p.Status = position.StatusClosed
		t := closedAt
		p.ClosedAt = &t
	}
	return nil
}

func (r *InMemoryPositionRepo) UpdateExcursion(_ context.Context, id int64, peak, trough float64, armed bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.byID[id]; ok {
		p.PeakUnrealizedJPY = peak
		p.TroughUnrealizedJPY = trough
		p.RatchetArmed = armed
	}
	return nil
}

// OPEN 限定 CAS。非 OPEN / 未知 id は (nil, nil)(pg の WHERE status='OPEN' と同じ契約)。
func (r *InMemoryPositionRepo) ExtendMaxHold(_ context.Context, id int64, addMinutes int) (*port.MaxHoldExtended, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok || p.Status != position.StatusOpen {
		return nil, nil
	}
	p.MaxHoldMinutes += addMinutes
	return &port.MaxHoldExtended{PositionID: id, NewMaxMinutes: p.MaxHoldMinutes}, nil
}

// reconcile で見つけた裸の broker 建玉を取り込む。HoldingMode は exec kind で決まる: 一日信用は誰が建てても
// 引け前フラット化の対象(持ち越しペナルティは無条件)、それ以外は multiday にして人間の現物/一般信用を
// bot が強制決済しないようにする。pg 版と必ず揃える。
func (r *InMemoryPositionRepo) AdoptExternal(_ context.Context, bp port.BrokerPosition, now time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	id := r.nextID
	r.byID[id] = &position.Position{
		ID: id, BrokerPositionID: bp.BrokerPositionID, Symbol: bp.Symbol, Side: bp.Side,
		Quantity: bp.Quantity, EntryPrice: bp.EntryPrice, ExecKind: bp.ExecKind,
		Source: position.SourceExternal, Status: position.StatusOpen, OpenedAt: now,
		HoldingMode: adoptedHoldingMode(bp.ExecKind),
	}
	return id, nil
}

func adoptedHoldingMode(ek order.ExecKind) order.HoldingMode {
	if ek == order.ExecMarginOneday {
		return order.HoldingIntraday
	}
	return order.HoldingMultiday
}

var _ port.PositionRepository = (*InMemoryPositionRepo)(nil)
