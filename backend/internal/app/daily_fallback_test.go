package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/testutil"
)

// klineCountingBroker counts GetKlines calls — the per-tick history fetch is the
// thing under test.
type klineCountingBroker struct {
	*broker.Paper
	calls atomic.Int64
}

func (k *klineCountingBroker) GetKlines(context.Context, string, port.KlinePeriod, int) ([]market.Candle, error) {
	k.calls.Add(1)
	return nil, nil
}

func newKlineCounter() *klineCountingBroker {
	return &klineCountingBroker{Paper: broker.NewPaper(clock.System(), 0, 0)}
}

// 日足は1日1本しか増えないのに、rawDailyCandles の broker フォールバックは
// **毎ティック(=毎秒)**評価される。candle repo が一時的に空/エラーを返すと
// 監視銘柄ぶんの履歴取得が毎秒走る。立花は履歴取得を PM18:00〜翌3:30 /
// AM5:30〜AM8:00 に限るよう案内しているので、窓の外では broker に落ちない。
func TestRawDailyCandlesSkipsBrokerFallbackOutsideHistoryWindow(t *testing.T) {
	ctx := context.Background()
	brk := newKlineCounter()
	b := &SymbolBundle{
		Symbol:  "7203",
		Broker:  brk,
		Candles: repository.NewInMemoryCandleRepo(), // 空 = フォールバック条件
		Clock:   clock.System(),
		Logger:  testutil.SilentLogger(),
		// 窓の外を表すことにする
		AllowHistoryFetch: func(time.Time) bool { return false },
	}
	for i := 0; i < 5; i++ {
		if cs := b.rawDailyCandles(ctx); len(cs) != 0 {
			t.Fatalf("窓外なのにバーが返った: %v", cs)
		}
	}
	if n := brk.calls.Load(); n != 0 {
		t.Fatalf("窓外で履歴取得を %d 回叩いた — 0 であるべき", n)
	}
}

// 窓の中では従来どおりフォールバックする(節約が機能欠落になってはいけない)。
func TestRawDailyCandlesUsesBrokerFallbackInsideHistoryWindow(t *testing.T) {
	ctx := context.Background()
	brk := newKlineCounter()
	b := &SymbolBundle{
		Symbol:            "7203",
		Broker:            brk,
		Candles:           repository.NewInMemoryCandleRepo(),
		Clock:             clock.System(),
		Logger:            testutil.SilentLogger(),
		AllowHistoryFetch: func(time.Time) bool { return true },
	}
	b.rawDailyCandles(ctx)
	if n := brk.calls.Load(); n != 1 {
		t.Fatalf("窓内でフォールバックしなかった (calls=%d)", n)
	}
}

// 述語を渡さない配線(既存テスト・backtest 等)は従来どおり素通し。
func TestRawDailyCandlesDefaultsToAllowingFallback(t *testing.T) {
	ctx := context.Background()
	brk := newKlineCounter()
	b := &SymbolBundle{
		Symbol:  "7203",
		Broker:  brk,
		Candles: repository.NewInMemoryCandleRepo(),
		Clock:   clock.System(),
		Logger:  testutil.SilentLogger(),
	}
	b.rawDailyCandles(ctx)
	if n := brk.calls.Load(); n != 1 {
		t.Fatalf("述語 nil で挙動が変わった (calls=%d)", n)
	}
}
