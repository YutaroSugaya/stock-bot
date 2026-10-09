package command

import (
	"context"
	"fmt"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// ManageOpenPositions is the per-tick exit engine. In live mode the broker-side
// OCO is the PRIMARY 守り; this is the time/path-based exit layer plus a backup
// for TP/SL, so it must never be treated as the only protection.
type ManageOpenPositions struct {
	exec             closeExecutor
	broker           port.Broker
	emergency        EmergencyController
	clock            clock.Clock
	maintenanceRatio float64
	// margin はトラック共有の口座照会キャッシュ(nil = 直に broker を叩く)。
	// 🛑 ブレーカーの照会は**間引かない** — ここは常に wire を打ち、その結果で
	// entry 経路のキャッシュを温める側。詳細は AccountMarginCache。
	margin *AccountMarginCache
	// split は権利落ち日の株式分割から建玉を守る(nil = 無効 = 従来どおり)。決済判定の前に通す。
	split *SplitGuard
}

// NewManageOpenPositions: maintenanceRatio<=0 disables the 維持率 breaker.
func NewManageOpenPositions(b port.Broker, pr port.PositionRepository, closer port.PositionCloser, em EmergencyController, c clock.Clock, maintenanceRatio float64) *ManageOpenPositions {
	if c == nil {
		c = clock.System()
	}
	return &ManageOpenPositions{
		exec:             closeExecutor{broker: b, posRepo: pr, closer: closer, emergency: em},
		broker:           b,
		emergency:        em,
		clock:            c,
		maintenanceRatio: maintenanceRatio,
	}
}

// WithAccountMargin はトラック共有の口座照会キャッシュを挿す。ブレーカーの実照会が
// そのまま entry 経路の入力になるので、保有中は entry 側の追加 wire がゼロになる。
func (m *ManageOpenPositions) WithAccountMargin(c *AccountMarginCache) *ManageOpenPositions {
	m.margin = c
	return m
}

// WithSplitGuard は株式分割の権利落ちガードを挿す。
func (m *ManageOpenPositions) WithSplitGuard(g *SplitGuard) *ManageOpenPositions {
	m.split = g
	return m
}

// WithCarry: 未設定なら CarryJPY は 0 のまま — 料率を捏造しない。
func (m *ManageOpenPositions) WithCarry(c position.CarryCalc) *ManageOpenPositions {
	m.exec.carry = c
	return m
}

// CheckMaintenance is SEPARATE from OnTick and price-independent: it queries the
// broker margin, not the tape, so it MUST run every tick even when the quote is
// stale or the ticker fetch failed — exactly the degraded-feed conditions where a
// 維持率 breach is most likely.
func (m *ManageOpenPositions) CheckMaintenance(ctx context.Context) {
	if m.maintenanceRatio <= 0 || m.emergency == nil {
		return
	}
	am, err := m.fetchMargin(ctx)
	if err == nil && am != nil && am.MarginRatio > 0 && am.MarginRatio < m.maintenanceRatio {
		_ = m.emergency.Trip("margin_maintenance_breach", m.clock())
	}
}

// fetchMargin は **必ず wire を打つ**。キャッシュがあるなら Refresh 経由で打ち、
// その結果を entry 経路へも配る(タダの相乗り)。
func (m *ManageOpenPositions) fetchMargin(ctx context.Context) (*order.AccountMargin, error) {
	if m.margin != nil {
		return m.margin.Refresh(ctx)
	}
	return m.broker.GetAccountMargin(ctx)
}

// OnTick runs only the PRICE-DEPENDENT exit checks; the 維持率 breaker lives in
// CheckMaintenance so it can run independently of a fresh quote.
func (m *ManageOpenPositions) OnTick(ctx context.Context, symbol string, summary *market.MarketSummary) error {
	now := m.clock()
	price := observedPrice(summary)
	if price <= 0 {
		return nil
	}
	positions, err := m.exec.posRepo.ListOpenOrClosing(ctx, symbol)
	if err != nil {
		return err
	}
	// 🛑 株式分割の権利落ちは**決済判定より先**に見る。分割前の円で凍結した SL を分割後の
	// 値段と比べると、含み益の建玉を損切りする。
	var splitHold map[int64]bool
	if m.split != nil {
		positions, splitHold = m.split.Screen(ctx, symbol, price, now, positions)
	}
	// 記録(peak/trough)の書込失敗は surface するが、**その tick の他の建玉の決済
	// 判定を止めてはいけない** — 計測器のために守りを落とすのは極性が逆。最初の 1 件
	// だけ持ち帰り、ループは回し切る。
	var deferredErr error
	for _, p := range positions {
		if p.Status != position.StatusOpen || p.Source == position.SourceExternal {
			continue // CLOSING handled elsewhere; external positions are not managed
		}
		if splitHold[p.ID] {
			continue // 分割を確かめられない建玉は、その日は自動決済しない(SplitGuard)
		}
		dec := position.EvaluateExit(p, price, now)
		if dec.ExcursionChanged {
			// 🛑 戻り値を捨てない。失敗すると peak/trough が静かに巻き戻り、arm 判定が
			// やり直しになる(決済しない方向なので危険側ではないが、ratchet が黙って
			// 劣化し、反実仮想の材料も欠ける)。決済する tick でも先に書く — SL に
			// 当たった瞬間の逆行が MAE の本命で、閉じた後には観測できない。
			if err := m.exec.posRepo.UpdateExcursion(ctx, p.ID, dec.NewPeak, dec.NewTrough, dec.NewArmed); err != nil && deferredErr == nil {
				deferredErr = fmt.Errorf("manage_open_positions: persist excursion (position %d): %w", p.ID, err)
			}
		}
		if dec.Exit {
			// 決済の失敗も同じ極性で扱う: 1 建玉の broker エラーで**同じ tick の他の
			// 建玉の決済判定を落とさない**(落ちた側は CLOSING のまま reconcile が拾い、
			// 守りを外した後の拒否なら closeOne が既に trip している)。
			if _, err := m.exec.closeOne(ctx, p, price, dec.Reason, now); err != nil && deferredErr == nil {
				deferredErr = err
			}
		}
	}
	return deferredErr
}

func observedPrice(summary *market.MarketSummary) float64 {
	if summary == nil {
		return 0
	}
	if summary.CurrentRate.Last > 0 {
		return summary.CurrentRate.Last
	}
	return (summary.CurrentRate.Bid + summary.CurrentRate.Ask) / 2
}
