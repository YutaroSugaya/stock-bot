package pg

import (
	"context"
	"stockbot/backend/internal/port"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PairTradeRow はペア差(`internal/backtest/pairdiff`)へ渡す 1 行。
//
// 🛑 **`trades` 単体では組めない。** `port.TradeRecord` は `ClosedAt` しか持たないが、
// ペアの同一性は**入口**で決まる —— 兄弟アームは同じトリガーで建ち、**出口だけが違う**
// (トレール側は伸ばすので決済日がずれるのが正常)。決済日で束ねると
// **設計どおりに動いているペアほど壊れる**。だから positions を join して
// `opened_at` と凍結済み `strategy_name`(migration 0015)を取る。
type PairTradeRow struct {
	Symbol     string
	Strategy   string
	EntryPrice float64
	Quantity   int
	NetJPY     float64
	OpenedAt   time.Time
	ClosedAt   time.Time
}

type PairRepo struct{ pool *pgxpool.Pool }

func NewPairRepo(pool *pgxpool.Pool) *PairRepo { return &PairRepo{pool: pool} }

// ListPairTrades は since(JST 日付の 0 時)以降に**決済された**トレードを、
// 入口の情報つきで返す。
//
// 🛑 **戦略の出口でない決済は除く**(`port.NonStrategyCloseReasons` — Go 側の
// `IsNonStrategyClose` と同じ集合をパラメータで渡す)。
// あれは巻き戻しと人間の決済で、出口の性能ではない —— 混ぜるとペア差が
// 「トレールが良いか」ではなく「事故がどちらに多く当たったか」になる。
// 🛑 `strategy_name` が空の行も除く: strategy_name を持たない旧建玉で、どちらのアームか判らない。
func (r *PairRepo) ListPairTrades(ctx context.Context, since time.Time) ([]PairTradeRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT p.symbol, p.strategy_name, p.entry_price, t.quantity,
		       t.profit_loss_jpy - t.fee_jpy + t.carry_jpy AS net_jpy,
		       p.opened_at, t.closed_at
		FROM trades t
		JOIN positions p ON p.id = t.position_id
		WHERE t.closed_at >= $1
		  AND p.strategy_name <> ''
		  AND NOT (t.close_reason = ANY($2::text[]))
		ORDER BY p.opened_at, p.symbol, p.strategy_name`, since, port.NonStrategyCloseReasons())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PairTradeRow
	for rows.Next() {
		var x PairTradeRow
		if err := rows.Scan(&x.Symbol, &x.Strategy, &x.EntryPrice, &x.Quantity,
			&x.NetJPY, &x.OpenedAt, &x.ClosedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// PairOpenRow は**まだ決済されていない**建玉。ペア差の突き合わせで
// 「兄弟脚がまだ建玉中」と「そもそも建たなかった」を分けるために要る。
type PairOpenRow struct {
	Symbol     string
	Strategy   string
	EntryPrice float64
	OpenedAt   time.Time
}

// ListOpenPairLegs は since 以降に**建てられて、まだ決済されていない**建玉。
//
// 🚨 これが無いと `cmd/pair-diff` は「片側しか決済トレードが無い」を
// **「片側しか建たなかった」**と読み、①ペア差が打ち切りで trail 不利に偏り
// ②コスト床検出器(BrokenByArm)が読めなくなる。MaxHold が 63 / 126 営業日の
// 2 ペアでは影響が支配的。
//
// 🛑 `strategy_name` が空の行は除く(どちらのアームか判らない・ListPairTrades と同じ作法)。
func (r *PairRepo) ListOpenPairLegs(ctx context.Context, since time.Time) ([]PairOpenRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT p.symbol, p.strategy_name, p.entry_price, p.opened_at
		FROM positions p
		WHERE p.status IN ('OPEN','CLOSING')
		  AND p.strategy_name <> ''
		  AND p.opened_at >= $1
		ORDER BY p.opened_at, p.symbol, p.strategy_name`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PairOpenRow
	for rows.Next() {
		var x PairOpenRow
		if err := rows.Scan(&x.Symbol, &x.Strategy, &x.EntryPrice, &x.OpenedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ArmRejection は 1 アームぶんの reject 件数(理由別)。
type ArmRejection struct {
	Strategy string
	Reason   string
	N        int
}

// CountRejectionsByArm は since 以降の `signal_rejections` を **アーム別 × 理由別**に数える。
//
// 🚨 事前登録した検出器の**読む側**。「`tp_below_cost_floor` をアーム別に
// 数える(黙って落とさない)」と書いてあるのに、行を書くところまでで**読む道具が
// 存在しなかった**。
//
// コスト床は trail 側だけ 3 倍厳しい(対象が 1.0×ATR で v2 は 3.0×ATR)ので、
// **trail 側の tp_below_cost_floor が構造的に多いはず**。その比が出せないと、
// 「ペアが揃ったものだけを見た結論」が高コスト銘柄を落とした後の話だと分からない。
//
// 🛑 戦略名は `strategy_configs` から join で引く。`signal_rejections` は config_id
// しか持たないので、join 無しではアーム別に割れない。
func (r *PairRepo) CountRejectionsByArm(ctx context.Context, since time.Time) ([]ArmRejection, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT COALESCE(NULLIF(sc.strategy_name, ''), '(戦略不明)') AS strategy,
		       -- not_loanable は銘柄名を含むので、理由の頭だけで束ねる。
		       split_part(sr.reason, ' ', 1) AS reason,
		       count(*) AS n
		FROM signal_rejections sr
		LEFT JOIN strategy_configs sc ON sc.config_id = sr.config_id
		WHERE sr.created_at >= $1
		GROUP BY 1, 2
		ORDER BY 1, 3 DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArmRejection
	for rows.Next() {
		var x ArmRejection
		if err := rows.Scan(&x.Strategy, &x.Reason, &x.N); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
