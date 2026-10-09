package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"stockbot/backend/internal/port"
)

// 日次総評 段1 の読み出し。**SELECT だけ** — このファイルに書込 SQL を足さない
// (cmd/daily-review が read-only であることの担保はここまで含めて成立する)。
//
// 期間はすべて半開区間 [from, to) で、境界の丸めは呼び手(usecase)が JST で行う。
// SQL 側で TZ 計算をしないのは、日跨ぎの解釈が 2 か所に分かれると必ずずれるため。
type JournalRepo struct{ pool *pgxpool.Pool }

func NewJournalRepo(pool *pgxpool.Pool) *JournalRepo { return &JournalRepo{pool: pool} }

func (r *JournalRepo) JournalClosedTrades(ctx context.Context, from, to time.Time) ([]port.JournalTrade, error) {
	rs, err := r.pool.Query(ctx, `
		SELECT t.symbol, COALESCE(sc.strategy_name, ''), t.side, t.quantity, t.entry_price, t.close_price,
		       t.profit_loss_jpy, t.fee_jpy, t.carry_jpy, t.close_reason, t.closed_at
		FROM trades t
		JOIN positions p ON p.id = t.position_id
		LEFT JOIN strategy_configs sc ON sc.config_id = p.config_id
		WHERE t.closed_at >= $1 AND t.closed_at < $2
		ORDER BY t.closed_at`, from, to)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []port.JournalTrade
	for rs.Next() {
		var t port.JournalTrade
		if err := rs.Scan(&t.Symbol, &t.Strategy, &t.Side, &t.Quantity, &t.EntryPrice, &t.ClosePrice,
			&t.GrossJPY, &t.FeeJPY, &t.CarryJPY, &t.CloseReason, &t.ClosedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rs.Err()
}

func (r *JournalRepo) JournalOpenedPositions(ctx context.Context, from, to time.Time) ([]port.JournalPosition, error) {
	return r.positions(ctx, `
		SELECT p.symbol, COALESCE(sc.strategy_name, ''), p.config_id, p.side, p.quantity, p.entry_price, p.opened_at,
		       COALESCE(p.take_profit_price,0), COALESCE(p.stop_loss_price,0),
		       COALESCE(p.peak_unrealized_jpy,0), COALESCE(p.trough_unrealized_jpy,0)
		FROM positions p
		LEFT JOIN strategy_configs sc ON sc.config_id = p.config_id
		WHERE p.opened_at >= $1 AND p.opened_at < $2
		ORDER BY p.opened_at`, from, to)
}

// 期間指定と時点指定で引数の数が違うので、可変長でそのまま渡す。

// JournalOpenPositionsAsOf は「その時刻に開いていた建玉」。**いま OPEN のもの**ではない —
// 遅れて走らせた日でも同じ数字が出るように、決済済みでも当時開いていれば数える。
func (r *JournalRepo) JournalOpenPositionsAsOf(ctx context.Context, at time.Time) ([]port.JournalPosition, error) {
	return r.positions(ctx, `
		SELECT p.symbol, COALESCE(sc.strategy_name, ''), p.config_id, p.side, p.quantity, p.entry_price, p.opened_at,
		       COALESCE(p.take_profit_price,0), COALESCE(p.stop_loss_price,0),
		       COALESCE(p.peak_unrealized_jpy,0), COALESCE(p.trough_unrealized_jpy,0)
		FROM positions p
		LEFT JOIN strategy_configs sc ON sc.config_id = p.config_id
		WHERE p.opened_at < $1 AND (p.closed_at IS NULL OR p.closed_at >= $1)
		ORDER BY p.opened_at`, at)
}

func (r *JournalRepo) positions(ctx context.Context, sql string, args ...any) ([]port.JournalPosition, error) {
	rs, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []port.JournalPosition
	for rs.Next() {
		var p port.JournalPosition
		if err := rs.Scan(&p.Symbol, &p.Strategy, &p.ConfigID, &p.Side, &p.Quantity, &p.EntryPrice, &p.OpenedAt,
			&p.TakeProfitPrice, &p.StopLossPrice, &p.PeakUnrealizedJPY, &p.TroughUnrealizedJPY); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rs.Err()
}

func (r *JournalRepo) JournalRejections(ctx context.Context, from, to time.Time) ([]port.JournalRejection, error) {
	rs, err := r.pool.Query(ctx, `
		SELECT reason, count(*) FROM signal_rejections
		WHERE created_at >= $1 AND created_at < $2
		GROUP BY reason`, from, to)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []port.JournalRejection
	for rs.Next() {
		var v port.JournalRejection
		if err := rs.Scan(&v.Reason, &v.N); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rs.Err()
}

// JournalScreens は戦略ごとに「何銘柄トリガーしたか / 何枠取れたか」と score 上位。
// **銘柄の重複を数えない**(1日に何ラウンド回っても銘柄数で数える)— ラウンド数が
// 変わると数字が跳ねる列を日記に出さないため。
func (r *JournalRepo) JournalScreens(ctx context.Context, from, to time.Time) ([]port.JournalScreen, error) {
	// 上位銘柄は **銘柄で重複排除**する。1日に何ラウンド回っても同じ銘柄が並ぶだけで、
	// 「上位5銘柄」が実質1銘柄になる(実測: 7911 が 5 つ並んだ)。
	rs, err := r.pool.Query(ctx, `
		SELECT s.strategy,
		       count(DISTINCT s.symbol) FILTER (WHERE s.triggered),
		       count(DISTINCT s.symbol) FILTER (WHERE s.picked),
		       COALESCE((SELECT array_agg(t.sym) FROM (
		           SELECT s2.symbol AS sym
		           FROM screen_snapshots s2
		           WHERE s2.strategy = s.strategy AND s2.round_at >= $1 AND s2.round_at < $2 AND s2.triggered
		           GROUP BY s2.symbol
		           ORDER BY max(s2.score) DESC
		           LIMIT 5) t), '{}')
		FROM screen_snapshots s
		WHERE s.round_at >= $1 AND s.round_at < $2 AND s.triggered
		GROUP BY s.strategy`, from, to)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []port.JournalScreen
	for rs.Next() {
		var v port.JournalScreen
		if err := rs.Scan(&v.Strategy, &v.Triggered, &v.Picked, &v.TopSymbols); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rs.Err()
}

var _ port.CounterfactualSource = (*JournalRepo)(nil)
var _ port.JournalSource = (*JournalRepo)(nil)

// ClosedPositions は反実仮想の入力。**凍結した TP/SL** を positions から読む
// (config の現在値ではない — 建玉時の約束が反実仮想の前提)。戦略名も凍結列
// `positions.strategy_name` を先に読み(PairRepo と同じ)、migration 0015 以前の行だけ
// strategy_configs の join に落ちる。
func (r *JournalRepo) ClosedPositions(ctx context.Context, from, to time.Time, reasons []string) ([]port.ClosedPositionSnapshot, error) {
	rs, err := r.pool.Query(ctx, `
		SELECT p.id, p.symbol, COALESCE(NULLIF(p.strategy_name, ''), sc.strategy_name, ''), p.side, p.quantity, p.entry_price,
		       p.take_profit_price, p.stop_loss_price, p.ratchet_arm_jpy,
		       p.ratchet_giveback_jpy, p.peak_unrealized_jpy, p.trough_unrealized_jpy, p.ratchet_floor_at_arm,
		       t.close_price, t.profit_loss_jpy - t.fee_jpy + t.carry_jpy, t.close_reason,
		       p.max_hold_minutes, p.opened_at, t.closed_at
		FROM trades t
		JOIN positions p ON p.id = t.position_id
		LEFT JOIN strategy_configs sc ON sc.config_id = p.config_id
		WHERE t.closed_at >= $1 AND t.closed_at < $2
		  AND ($3::text[] IS NULL OR cardinality($3::text[]) = 0 OR t.close_reason = ANY($3::text[]))
		ORDER BY t.closed_at`, from, to, reasons)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []port.ClosedPositionSnapshot
	for rs.Next() {
		var s port.ClosedPositionSnapshot
		if err := rs.Scan(&s.PositionID, &s.Symbol, &s.Strategy, &s.Side, &s.Quantity, &s.EntryPrice,
			&s.TakeProfitPrice, &s.StopLossPrice, &s.RatchetArmJPY,
			&s.RatchetGivebackJPY, &s.PeakUnrealizedJPY, &s.TroughUnrealizedJPY, &s.RatchetFloorAtArm,
			&s.ClosePrice, &s.NetJPY, &s.CloseReason,
			&s.MaxHoldMinutes, &s.OpenedAt, &s.ClosedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rs.Err()
}
