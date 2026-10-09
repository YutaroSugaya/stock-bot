package pg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// *BySymbol 族は per-symbol gate を隣の銘柄から隔離する。日次損失は net(gross - fee + carry)判定で、
// 手数料が損失を深くするぶん gross より早く cap を落とす(保守側)。in-memory 版と必ず揃える。
type TradeRepo struct{ pool *pgxpool.Pool }

func NewTradeRepo(pool *pgxpool.Pool) *TradeRepo { return &TradeRepo{pool: pool} }

func (r *TradeRepo) Insert(ctx context.Context, t port.TradeRecord) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO trades (position_id, symbol, side, quantity, entry_price, close_price,
			profit_loss_jpy, fee_jpy, carry_jpy, fee_estimated, close_reason, closed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		t.PositionID, t.Symbol, string(t.Side), t.Quantity, t.EntryPrice, t.ClosePrice,
		t.ProfitLossJPY, t.FeeJPY, t.CarryJPY, t.FeeEstimated, t.CloseReason, t.ClosedAt,
	).Scan(&id)
	return id, err
}

func (r *TradeRepo) SumClosedLossJPYSinceBySymbol(ctx context.Context, symbol string, since time.Time) (int, error) {
	var sum float64
	err := r.pool.QueryRow(ctx, `
		SELECT coalesce(sum(-(profit_loss_jpy - fee_jpy + carry_jpy)),0) FROM trades
		WHERE symbol=$1 AND closed_at>=$2 AND (profit_loss_jpy - fee_jpy + carry_jpy)<0`, symbol, since).Scan(&sum)
	return int(sum), err
}

// 🚨 **per-symbol の再入場ゲート 3 本(窓の取引回数 / 連敗 / cooldown)は
// external_close を数えない**(port.CountsTowardEntryGates)。人間が同じ銘柄で勝って
// 決済すると、それが最新行になって bot の連敗 streak を切り cooldown を無効化する =
// **人間の売買で bot の安全ゲートが解除される**。日次損失の合計(下の 2 本)は逆に
// **両方数える** — 委託保証金・維持率には人間の損失も効くので締める側は落とさない。
func (r *TradeRepo) CountTradesSinceBySymbol(ctx context.Context, symbol string, since time.Time) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM trades WHERE symbol=$1 AND closed_at>=$2 AND close_reason<>$3`,
		symbol, since, port.CloseReasonExternalClose).Scan(&n)
	return n, err
}

func (r *TradeRepo) ConsecutiveLossesBySymbol(ctx context.Context, symbol string) (int, error) {
	rows, err := r.pool.Query(ctx, `SELECT profit_loss_jpy FROM trades WHERE symbol=$1 AND close_reason<>$2 ORDER BY closed_at DESC, id DESC`,
		symbol, port.CloseReasonExternalClose)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	streak := 0
	for rows.Next() {
		var pnl float64
		if err := rows.Scan(&pnl); err != nil {
			return 0, err
		}
		if pnl < 0 {
			streak++
		} else {
			break
		}
	}
	return streak, rows.Err()
}

func (r *TradeRepo) LastCloseBySymbol(ctx context.Context, symbol string) (*port.LastClose, error) {
	var out port.LastClose
	err := r.pool.QueryRow(ctx, `SELECT closed_at, profit_loss_jpy - fee_jpy + carry_jpy
		FROM trades WHERE symbol=$1 AND close_reason<>$2 ORDER BY closed_at DESC LIMIT 1`,
		symbol, port.CloseReasonExternalClose).Scan(&out.ClosedAt, &out.NetJPY)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// 並びは in-memory 版と揃える(ORDER BY closed_at, id)。
func (r *TradeRepo) ListClosedSince(ctx context.Context, since time.Time) ([]port.TradeRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT position_id, symbol, side, quantity, entry_price, close_price,
			profit_loss_jpy, fee_jpy, carry_jpy, fee_estimated, close_reason, closed_at
		FROM trades WHERE closed_at>=$1 ORDER BY closed_at, id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []port.TradeRecord
	for rows.Next() {
		var t port.TradeRecord
		var side string
		if err := rows.Scan(&t.PositionID, &t.Symbol, &side, &t.Quantity, &t.EntryPrice, &t.ClosePrice,
			&t.ProfitLossJPY, &t.FeeJPY, &t.CarryJPY, &t.FeeEstimated, &t.CloseReason, &t.ClosedAt); err != nil {
			return nil, err
		}
		t.Side = order.Side(side)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *TradeRepo) SumClosedLossJPYSince(ctx context.Context, since time.Time) (int, error) {
	var sum float64
	err := r.pool.QueryRow(ctx, `
		SELECT coalesce(sum(-(profit_loss_jpy - fee_jpy + carry_jpy)),0) FROM trades
		WHERE closed_at>=$1 AND (profit_loss_jpy - fee_jpy + carry_jpy)<0`, since).Scan(&sum)
	return int(sum), err
}

var _ port.TradeRepository = (*TradeRepo)(nil)
