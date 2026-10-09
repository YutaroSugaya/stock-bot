package repository

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/port"
)

// 🛑 **株式分割は「過去のバーを遡って書き換える」操作**。取り込みが
// ON CONFLICT DO NOTHING だと、chain-link 済みの調整値が**永久に DB へ入らない**。
//
// 実害の形: ある銘柄が 1:4 分割し、CSV は
// `market.ChainLinkSplits` で正しく調整されていたのに DB は未調整のまま
// (前日 6842 / 当日 1672)。結果:
//   - 25日線が 4,887(実勢 1,700 前後)になり、乖離 **-65%** の偽の大暴落に見えた
//   - BNF(逆張り)がそれを最良のエントリー候補として選び、**live トラックが arm した**
//   - ATR も 436円(実勢 約30円)に膨らみ、SL=2×ATR が建値の 50% になって
//     fat-finger backstop に弾かれた ← **これだけが live の誤発注を止めていた**
//
// CSV は正本、DB はその写し。写しが更新できない設計は「直したのに直らない」を生む。
func TestCandleUpsertOverwritesExistingBar(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryCandleRepo()
	at := time.Date(2026, 7, 29, 0, 0, 0, 0, clock.JST)

	unadjusted := []market.Candle{{OpenTime: at, Open: 6800, High: 6900, Low: 6700, Close: 6842, Volume: 1000, Interval: 24 * time.Hour}}
	if err := repo.Upsert(ctx, "8309", unadjusted); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 分割検出後に chain-link した値で取り込み直す(= 実際の fetch-daily の挙動)。
	adjusted := []market.Candle{{OpenTime: at, Open: 1700, High: 1725, Low: 1675, Close: 1710.5, Volume: 4000, Interval: 24 * time.Hour}}
	if err := repo.Upsert(ctx, "8309", adjusted); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	got, err := repo.List(ctx, "8309", port.PeriodDaily, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("バーが %d 本(同一 open_time は 1 本に畳まれるべき)", len(got))
	}
	if got[0].Close != 1710.5 {
		t.Errorf("close=%v, want 1710.5 — 分割調整が DB に入らない(DO NOTHING のまま)", got[0].Close)
	}
	if got[0].Volume != 4000 {
		t.Errorf("volume=%v, want 4000(出来高も分割比で調整される)", got[0].Volume)
	}
}
