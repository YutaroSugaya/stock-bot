package pg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

type PositionRepo struct{ pool *pgxpool.Pool }

func NewPositionRepo(pool *pgxpool.Pool) *PositionRepo { return &PositionRepo{pool: pool} }

const positionColumns = `id, broker_position_id, symbol, side, quantity, entry_price,
	take_profit_jpy, stop_loss_jpy, take_profit_price, stop_loss_price,
	max_hold_minutes, ratchet_arm_jpy, ratchet_giveback_jpy,
	extension_max_minutes, extension_unrealized_jpy, early_exit_window_minutes, early_exit_target_jpy,
	peak_unrealized_jpy, trough_unrealized_jpy, ratchet_armed, ratchet_floor_at_arm, config_id, strategy_name, holding_mode, exec_kind, tick_size_at_entry,
	source, status, opened_at, closed_at, COALESCE(entry_fee_jpy, 0),
	split_factor, to_char(split_adjusted_on, 'YYYY-MM-DD')`

func (r *PositionRepo) Insert(ctx context.Context, in port.PositionInsertInput) (int64, error) {
	src := in.Source
	if src == "" {
		src = position.SourceBot
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO positions (broker_position_id, symbol, side, quantity, entry_price,
			take_profit_jpy, stop_loss_jpy, take_profit_price, stop_loss_price,
			max_hold_minutes, ratchet_arm_jpy, ratchet_giveback_jpy,
			extension_max_minutes, extension_unrealized_jpy, early_exit_window_minutes, early_exit_target_jpy,
			config_id, strategy_name, holding_mode, exec_kind, tick_size_at_entry, source, entry_fee_jpy, ratchet_floor_at_arm, status, opened_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,'OPEN',$25)
		RETURNING id`,
		nullStr(in.BrokerPositionID), in.Symbol, string(in.Side), in.Quantity, in.EntryPrice,
		in.TakeProfitJPY, in.StopLossJPY, in.TakeProfitPrice, in.StopLossPrice,
		in.MaxHoldMinutes, in.RatchetArmJPY, in.RatchetGivebackJPY,
		in.ExtensionMaxMinutes, in.ExtensionUnrealizedJPY, in.EarlyExitWindowMinutes, in.EarlyExitTargetJPY,
		in.StrategyConfigID, in.StrategyName, string(in.HoldingMode), string(in.ExecKind), in.TickSizeAtEntry, string(src),
		in.EntryFeeJPY, in.RatchetFloorAtArm, in.OpenedAt,
	).Scan(&id)
	return id, err
}

func (r *PositionRepo) ListOpenOrClosing(ctx context.Context, symbol string) ([]position.Position, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+positionColumns+`
		FROM positions WHERE symbol=$1 AND status IN ('OPEN','CLOSING') ORDER BY id`, symbol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []position.Position
	for rows.Next() {
		p, err := scanPosition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// 起動時の紙帳簿復元に使う。ユニバースから外れた銘柄も取りこぼさないため全銘柄を返す。
func (r *PositionRepo) ListOpenAllSymbols(ctx context.Context) ([]position.Position, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+positionColumns+`
		FROM positions WHERE status IN ('OPEN','CLOSING') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []position.Position
	for rows.Next() {
		p, err := scanPosition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// 未知 id は (nil, nil)。
func (r *PositionRepo) GetByID(ctx context.Context, id int64) (*position.Position, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+positionColumns+` FROM positions WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	p, err := scanPosition(rows)
	if err != nil {
		return nil, err
	}
	return &p, rows.Err()
}

// external adoption も数える(委託保証金の保護対象なので除外しない)。
func (r *PositionRepo) CountOpenAllSymbols(ctx context.Context) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM positions WHERE status IN ('OPEN','CLOSING')`).Scan(&n)
	return n, err
}

// UpdateProtectivePrices は OPEN 建玉の台帳の TP/SL(価格と幅)を人間が変えた値へ揃える。
func (r *PositionRepo) UpdateProtectivePrices(ctx context.Context, id int64, tpPrice, slPrice, tpJPY, slJPY float64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE positions
		SET take_profit_price=$2, stop_loss_price=$3, take_profit_jpy=$4, stop_loss_jpy=$5
		WHERE id=$1 AND status='OPEN'`, id, tpPrice, slPrice, tpJPY, slJPY)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ApplySplit は権利落ち日の分割調整(position.SplitAdjusted の結果)を OPEN 建玉へ書く。
// CAS: OPEN かつ**その日にまだ調整していない**行だけ(同じ日に二度割らない)。
func (r *PositionRepo) ApplySplit(ctx context.Context, adj position.Position) (bool, error) {
	if adj.SplitAdjustedOn.IsZero() {
		return false, errors.New("ApplySplit: 権利落ち日(SplitAdjustedOn)が空")
	}
	day := adj.SplitAdjustedOn.Format(time.DateOnly)
	tag, err := r.pool.Exec(ctx, `UPDATE positions
		SET quantity=$2, entry_price=$3, take_profit_price=$4, stop_loss_price=$5,
		    take_profit_jpy=$6, stop_loss_jpy=$7, extension_unrealized_jpy=$8, early_exit_target_jpy=$9,
		    ratchet_arm_jpy=$10, ratchet_giveback_jpy=$11, peak_unrealized_jpy=$12, trough_unrealized_jpy=$13,
		    split_factor=$14, split_adjusted_on=$15::date
		WHERE id=$1 AND status='OPEN' AND split_adjusted_on IS DISTINCT FROM $15::date`,
		adj.ID, adj.Quantity, adj.EntryPrice, adj.TakeProfitPrice, adj.StopLossPrice,
		adj.TakeProfitJPY, adj.StopLossJPY, adj.ExtensionUnrealizedJPY, adj.EarlyExitTargetJPY,
		adj.RatchetArmJPY, adj.RatchetGivebackJPY, adj.PeakUnrealizedJPY, adj.TroughUnrealizedJPY,
		adj.SplitFactor, day)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CountOpenedSinceBySymbolStrategy は since 以降に (銘柄, 戦略) で建てた bot 建玉の数(状態を問わない)。
func (r *PositionRepo) CountOpenedSinceBySymbolStrategy(ctx context.Context, symbol, strategy string, since time.Time) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM positions
		WHERE symbol=$1 AND strategy_name=$2 AND source='bot' AND opened_at >= $3`, symbol, strategy, since).Scan(&n)
	return n, err
}

// CountOpenedSince は since 以降に口座全体で建てた bot 建玉の数(状態を問わない)。
func (r *PositionRepo) CountOpenedSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM positions
		WHERE source='bot' AND opened_at >= $1`, since).Scan(&n)
	return n, err
}

// CountOpenAcross は本数・銘柄数・入口の銘柄数を **1 クエリ**で返す。
// external adoption も数える(監視集合に乗るので、枠の判断からは外せない)。
//
// 🛑 入口の判定に `strategy_name` の正規表現を使わない(`_trail` を剥がす規則は
// domain の `strategy.EntryArmOf` が正本)。呼び手が基と兄弟の 2 名を渡す。
func (r *PositionRepo) CountOpenAcross(ctx context.Context, entryArms []string) (port.OpenCounts, error) {
	if entryArms == nil {
		entryArms = []string{}
	}
	var out port.OpenCounts
	err := r.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(DISTINCT symbol),
		       count(DISTINCT symbol) FILTER (WHERE strategy_name = ANY($1))
		  FROM positions
		 WHERE status IN ('OPEN','CLOSING')`, entryArms).
		Scan(&out.Positions, &out.Symbols, &out.EntryArmSymbols)
	return out, err
}

// CAS: OPEN だけが CLOSING に倒せる(二重決済防止)。
func (r *PositionRepo) ClaimForClose(ctx context.Context, id int64, _ time.Time) (bool, error) {
	ct, err := r.pool.Exec(ctx, `UPDATE positions SET status='CLOSING' WHERE id=$1 AND status='OPEN'`, id)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() == 1, nil
}

func (r *PositionRepo) MarkClosed(ctx context.Context, id int64, closedAt time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE positions SET status='CLOSED', closed_at=$2 WHERE id=$1`, id, closedAt)
	return err
}

func (r *PositionRepo) UpdateExcursion(ctx context.Context, id int64, peak, trough float64, armed bool) error {
	_, err := r.pool.Exec(ctx, `UPDATE positions SET peak_unrealized_jpy=$2, trough_unrealized_jpy=$3, ratchet_armed=$4 WHERE id=$1`, id, peak, trough, armed)
	return err
}

// OPEN 限定 CAS。非 OPEN / 未知 id は (nil, nil) → handler が 404。
func (r *PositionRepo) ExtendMaxHold(ctx context.Context, id int64, addMinutes int) (*port.MaxHoldExtended, error) {
	var newMax int
	err := r.pool.QueryRow(ctx, `UPDATE positions SET max_hold_minutes=max_hold_minutes+$2 WHERE id=$1 AND status='OPEN' RETURNING max_hold_minutes`, id, addMinutes).Scan(&newMax)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &port.MaxHoldExtended{PositionID: id, NewMaxMinutes: newMax}, nil
}

// reconcile で見つけた裸の broker 建玉を取り込む。HoldingMode は exec kind で決まる: 一日信用 → intraday
// (引け前フラット化の対象)、それ以外 → multiday(人間の建玉を bot が強制決済しない)。in-memory 版と必ず揃える。
func (r *PositionRepo) AdoptExternal(ctx context.Context, bp port.BrokerPosition, now time.Time) (int64, error) {
	holding := "multiday"
	if bp.ExecKind == order.ExecMarginOneday {
		holding = "intraday"
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO positions (broker_position_id, symbol, side, quantity, entry_price,
			config_id, holding_mode, exec_kind, tick_size_at_entry, source, status, opened_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,0,'external_broker','OPEN',$9)
		RETURNING id`,
		nullStr(bp.BrokerPositionID), bp.Symbol, string(bp.Side), bp.Quantity, bp.EntryPrice,
		externalConfigID, holding, string(bp.ExecKind), now,
	).Scan(&id)
	return id, err
}

// external adoption が FK 参照する番兵。存在保証は起動時の main 側。
const externalConfigID = "external_adoption"

func scanPosition(rows pgx.Rows) (position.Position, error) {
	var p position.Position
	var bpID, closedAt, splitOn any
	var side, holding, exec, src, status, stratName string
	err := rows.Scan(
		&p.ID, &bpID, &p.Symbol, &side, &p.Quantity, &p.EntryPrice,
		&p.TakeProfitJPY, &p.StopLossJPY, &p.TakeProfitPrice, &p.StopLossPrice,
		&p.MaxHoldMinutes, &p.RatchetArmJPY, &p.RatchetGivebackJPY,
		&p.ExtensionMaxMinutes, &p.ExtensionUnrealizedJPY, &p.EarlyExitWindowMinutes, &p.EarlyExitTargetJPY,
		&p.PeakUnrealizedJPY, &p.TroughUnrealizedJPY, &p.RatchetArmed, &p.RatchetFloorAtArm, &p.StrategyConfigID, &stratName, &holding, &exec, &p.TickSizeAtEntry,
		&src, &status, &p.OpenedAt, &closedAt, &p.EntryFeeJPY,
		&p.SplitFactor, &splitOn,
	)
	if err != nil {
		return p, err
	}
	if s, ok := bpID.(string); ok {
		p.BrokerPositionID = s
	}
	if t, ok := closedAt.(time.Time); ok {
		p.ClosedAt = &t
	}
	if d, ok := splitOn.(string); ok && d != "" {
		t, perr := time.Parse(time.DateOnly, d)
		if perr != nil {
			return p, perr
		}
		p.SplitAdjustedOn = t
	}
	p.StrategyName = stratName
	p.Side = order.Side(side)
	p.HoldingMode = order.HoldingMode(holding)
	p.ExecKind = order.ExecKind(exec)
	p.Source = position.Source(src)
	p.Status = position.Status(status)
	return p, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// 建玉時の戦略名を引く。正は建玉に凍結した strategy_name(migration 0015)で、空の行
// (external・0015 前の旧建玉)だけ凍結 config_id の join に落とす。どちらも無ければ
// "unknown"(隠すより見せる)。
func (r *PositionRepo) StrategyByPositionID(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT p.id, COALESCE(NULLIF(p.strategy_name, ''), sc.strategy_name, 'unknown')
		FROM positions p
		LEFT JOIN strategy_configs sc ON sc.config_id = p.config_id
		WHERE p.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

var _ port.PositionRepository = (*PositionRepo)(nil)
var _ port.TradeStrategyResolver = (*PositionRepo)(nil)
