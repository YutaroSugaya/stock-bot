package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

type CandleRepo struct{ pool *pgxpool.Pool }

func NewCandleRepo(pool *pgxpool.Pool) *CandleRepo { return &CandleRepo{pool: pool} }

// (symbol, interval, open_time) の重複は無視する = 定期再取得が冪等。
func (r *CandleRepo) Upsert(ctx context.Context, symbol string, candles []market.Candle) error {
	if len(candles) == 0 {
		return nil
	}
	batch := make([][]any, 0, len(candles))
	for _, c := range candles {
		batch = append(batch, []any{symbol, intervalLabel(c.Interval), c.OpenTime, c.Open, c.High, c.Low, c.Close, c.Volume})
	}
	for _, row := range batch {
		// 🛑 **DO UPDATE**。株式分割は「過去のバーを遡って書き換える」操作なので、
		// DO NOTHING だと chain-link 済みの調整値が永久に DB へ入らない。
		// 実害の例: 1:4 分割の銘柄で CSV は調整済みなのに DB は未調整のまま
		// 残り、25日線が 4,887(実勢 1,700)= 乖離 -65% の偽の暴落になって
		// BNF(逆張り)が live トラックで arm した。
		// 未調整の broker バーで上書きされる心配は無い: refreshDailyCandles が
		// discontinuityVs で断裂した取得を upsert 前に捨てている。
		if _, err := r.pool.Exec(ctx, `
			INSERT INTO candles (symbol, interval, open_time, open, high, low, close, volume)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (symbol, interval, open_time) DO UPDATE SET
				open = EXCLUDED.open, high = EXCLUDED.high, low = EXCLUDED.low,
				close = EXCLUDED.close, volume = EXCLUDED.volume`, row...); err != nil {
			return err
		}
	}
	return nil
}

func (r *CandleRepo) List(ctx context.Context, symbol string, period port.KlinePeriod, limit int) ([]market.Candle, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := r.pool.Query(ctx, `
		SELECT open_time, open, high, low, close, volume FROM candles
		WHERE symbol=$1 AND interval=$2 ORDER BY open_time DESC LIMIT $3`, symbol, string(period), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rev []market.Candle
	for rows.Next() {
		c := market.Candle{Symbol: symbol, Interval: intervalFromLabel(string(period))}
		if err := rows.Scan(&c.OpenTime, &c.Open, &c.High, &c.Low, &c.Close, &c.Volume); err != nil {
			return nil, err
		}
		rev = append(rev, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 時系列順に反転
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

func intervalLabel(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return string(port.PeriodDaily)
	case d >= time.Hour:
		return string(port.Period1h)
	case d >= 5*time.Minute:
		return string(port.Period5m)
	default:
		return string(port.Period1m)
	}
}

func intervalFromLabel(label string) time.Duration {
	switch port.KlinePeriod(label) {
	case port.PeriodDaily:
		return 24 * time.Hour
	case port.Period1h:
		return time.Hour
	case port.Period5m:
		return 5 * time.Minute
	default:
		return time.Minute
	}
}

var _ port.CandleRepository = (*CandleRepo)(nil)
