package repository

import (
	"context"
	"sort"
	"time"

	"stockbot/backend/internal/port"
)

// TradeSource は合算する台帳 1 本(= 1 DB)。Strategies は nil 可(戦略名を返さない)。
type TradeSource struct {
	Trades     port.ClosedTradeReader
	Strategies port.TradeStrategyResolver
}

// MergedTrades は複数 DB の決済台帳を **読むだけ**で合算する(画面の「全期間」の戦績)。
// 期間ごとに DB を切っているので、1 本の repo では全期間が出せない。
//
// 🛑 **建玉 id は DB ごとに 1 から振られて衝突する**。合算後の id は上位ビットにソースの
// 番号を載せて名前空間を分け、戦略名の解決は元のソースへ id を戻して問い合わせる。
// 書込系は持たない(port.ClosedTradeReader / port.TradeStrategyResolver だけを満たす)。
type MergedTrades struct {
	srcs []TradeSource
}

// mergedIDShift: 元の id はこのビット数未満であること(1 兆件超の建玉は想定しない)。
const mergedIDShift = 40

func NewMergedTrades(srcs ...TradeSource) *MergedTrades {
	return &MergedTrades{srcs: srcs}
}

func (m *MergedTrades) ListClosedSince(ctx context.Context, since time.Time) ([]port.TradeRecord, error) {
	var out []port.TradeRecord
	for i, s := range m.srcs {
		recs, err := s.Trades.ListClosedSince(ctx, since)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			r.PositionID = int64(i)<<mergedIDShift | r.PositionID
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].ClosedAt.Before(out[b].ClosedAt) })
	return out, nil
}

func (m *MergedTrades) StrategyByPositionID(ctx context.Context, ids []int64) (map[int64]string, error) {
	bySrc := map[int][]int64{}
	for _, id := range ids {
		i := int(id >> mergedIDShift)
		bySrc[i] = append(bySrc[i], id&(1<<mergedIDShift-1))
	}
	out := map[int64]string{}
	for i, local := range bySrc {
		if i < 0 || i >= len(m.srcs) || m.srcs[i].Strategies == nil {
			continue
		}
		names, err := m.srcs[i].Strategies.StrategyByPositionID(ctx, local)
		if err != nil {
			return nil, err
		}
		for id, n := range names {
			out[int64(i)<<mergedIDShift|id] = n
		}
	}
	return out, nil
}
