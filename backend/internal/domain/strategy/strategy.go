package strategy

import (
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/session"
)

type EvalInput struct {
	Now          time.Time
	Summary      *market.MarketSummary
	Candles1m    []market.Candle
	Candles5m    []market.Candle
	Candles1h    []market.Candle
	CandlesDaily []market.Candle
	Config       *config.StrategyConfig

	// Hours は戦略別 MaxHold(入口の窓 × 0.5 **営業日**)を暦の分数へ
	// 直すための休場カレンダー。domain 内の純粋な値なので層規約は破らない。
	//
	// zero 値(TZ nil)でも期限は付く — `AddBusinessDays` が週末だけを飛ばす営業日に
	// 縮退する(祝日を知らないぶん実際より手前に落ちる)。**「無期限」には縮退させない**:
	// backtest の日足リプレイは Hours を持たないので、そこで検定が静かに消える。
	Hours session.TradingHours
}

// 銘柄不明()なら呼値は粗いテーブルへ倒れる(fail-safe)。
func evalSymbol(in EvalInput) string {
	if in.Summary == nil {
		return ""
	}
	return in.Summary.Symbol
}

// tick 建ての床を円/株へ直す係数。0 = 呼値不明で床の比較をしない。
func summaryTickSize(in EvalInput) float64 {
	if in.Summary == nil {
		return 0
	}
	if in.Summary.TickSize > 0 {
		return in.Summary.TickSize
	}
	return market.TickSizeOf(in.Summary.Symbol, in.Summary.CurrentRate.Last)
}

// Evaluate は純粋(I/O・DB・rand なし)。price tick 毎に呼べ、backtest が決定的になる。
type Strategy interface {
	Name() config.StrategyName
	Evaluate(in EvalInput) Signal
}
