package app

import (
	"context"
	"errors"
	"testing"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/testutil"
)

// 🚨 実弾がこのテストの理由: 口座のレバ余力が候補 1 本の金額を下回る状態で
// selector がその候補(8001)を arm した。**arm した時点で成立しえない**ので、
// 場中ずっと「毎ティック entry シグナル → 構造ゲート通過 → 口座照会 → gross_notional_cap
// で reject」を繰り返し、後場だけで wire を数千回焼いた。
//
// 🛑 CLAUDE.md が risk_per_trade / 貸借銘柄について定める「**枠を配る前にも落とす**」
// と同じ形。余力は口座単位なので、**候補ごとの判定より先に**「そもそも 1 本でも
// 建てられるのか」を見る。

type stubMargin struct {
	equity float64
	err    error
}

func (s stubMargin) Get(context.Context) (*order.AccountMargin, bool) {
	if s.err != nil {
		return nil, false
	}
	return &order.AccountMargin{Equity: s.equity, AvailableJPY: s.equity}, true
}

// leverageFixture: 建玉 1 単元 = 2,000 × 100 = 200,000円 の候補 1 本。
// openGrossJPY は **holders の外**の銘柄で積む(建玉中の銘柄は arm 対象から外れるため)。
func leverageFixture(t *testing.T, openGrossJPY int) (*Selector, map[string]*ActiveConfigHolder) {
	t.Helper()
	ctx := context.Background()
	candles := repository.NewInMemoryCandleRepo()
	if err := candles.Upsert(ctx, "AAAA", calmSeries(30)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	holders := map[string]*ActiveConfigHolder{"AAAA": NewActiveConfigHolder(noTradeCfgFor("AAAA"))}
	pos := repository.NewInMemoryPositionRepo()
	if openGrossJPY > 0 {
		if _, err := pos.Insert(ctx, port.PositionInsertInput{
			Symbol: "ZZZZ", Side: order.SideBuy, Quantity: 1, EntryPrice: float64(openGrossJPY),
			StrategyConfigID: "seed", HoldingMode: order.HoldingMultiday,
		}); err != nil {
			t.Fatalf("seed position: %v", err)
		}
	}
	armCfg := func(sym string) *config.StrategyConfig {
		c := &config.StrategyConfig{ConfigID: "armed_" + sym, Symbol: sym,
			StrategyName: config.StrategyBNFReversion, HoldingMode: config.HoldingMultiday}
		c.Risk.Quantity = 100
		return c
	}
	sel := NewSelector(candles, pos, holders,
		[]strategy.Screener{plannedStopScreener{}}, 10, 0,
		armCfg, noTradeCfgFor, nil, testutil.SilentLogger())
	return sel, holders
}

// 保証金 560,000 × 2.0 = 1,120,000 が上限。建玉 1,100,000 なら余力 20,000 で、
// 200,000 の 1 単元は入らない。**arm しない**のが正しい(枠を占有させない)。
func TestSelector_DoesNotArmWhenOneLotExceedsLeverageHeadroom(t *testing.T) {
	sel, holders := leverageFixture(t, 1_100_000)
	sel.WithLeverageHeadroom(stubMargin{equity: 560000}, 2.0)
	sel.Tick(context.Background())

	if got := armedStrategy(holders["AAAA"]); got != config.StrategyNoTrade {
		t.Fatalf("余力 20,000 に対し 1 単元 200,000 を arm した: %v", got)
	}
	if s := sel.LeverageState(); s != LeverageOK {
		t.Fatalf("余力自体は残っている(20,000)ので state=%q ではなく %q のはず", s, LeverageOK)
	}
	if got := sel.LeverageHeadroomJPY(); got != 20000 {
		t.Fatalf("余力の表示が %d (want 20000)", got)
	}
}

// 建玉が上限に達している = **資金枠なし(枠待ち)**。1 銘柄も arm しない。
func TestSelector_FundlessWhenNoLeverageHeadroomAtAll(t *testing.T) {
	sel, holders := leverageFixture(t, 1_200_000)
	sel.WithLeverageHeadroom(stubMargin{equity: 560000}, 2.0)
	sel.Tick(context.Background())

	if got := armedStrategy(holders["AAAA"]); got != config.StrategyNoTrade {
		t.Fatalf("資金枠が無いのに arm した: %v", got)
	}
	if s := sel.LeverageState(); s != LeverageFundless {
		t.Fatalf("state=%q (want %q)", s, LeverageFundless)
	}
	if got := sel.LeverageHeadroomJPY(); got >= 0 {
		t.Fatalf("余力が %d — 上限超なら負で出す(枠待ちの深さが読めない)", got)
	}
}

// 余力があるなら従来どおり arm する。
func TestSelector_ArmsWhenLeverageHeadroomCoversOneLot(t *testing.T) {
	sel, holders := leverageFixture(t, 0)
	sel.WithLeverageHeadroom(stubMargin{equity: 560000}, 2.0)
	sel.Tick(context.Background())

	if got := armedStrategy(holders["AAAA"]); got != config.StrategyBNFReversion {
		t.Fatalf("余力 1,120,000 に対し 1 単元 200,000 は arm されるべき: %v", got)
	}
	if s := sel.LeverageState(); s != LeverageOK {
		t.Fatalf("state=%q (want %q)", s, LeverageOK)
	}
}

// 🛑 照会できないときは **arm しない**(fail-close)。
// withinRiskPerTrade の「未算出は通す」と逆に倒すのは、あちらが「標本が理由の分からない
// まま消える」ことを避ける判断なのに対し、こちらは通しても**発注前ゲートが
// margin_status_unavailable で必ず落とす**から。通すと同じ空回りになる。
func TestSelector_DoesNotArmWhenMarginIsUnknown(t *testing.T) {
	sel, holders := leverageFixture(t, 0)
	sel.WithLeverageHeadroom(stubMargin{err: errors.New("session inactive")}, 2.0)
	sel.Tick(context.Background())

	if got := armedStrategy(holders["AAAA"]); got != config.StrategyNoTrade {
		t.Fatalf("余力不明なのに arm した: %v", got)
	}
	if s := sel.LeverageState(); s != LeverageUnknown {
		t.Fatalf("state=%q (want %q)", s, LeverageUnknown)
	}
}

// レバ上限を持たない構成(research)は従来どおり。口座照会も一度も打たない。
func TestSelector_LeverageFilterIsOffWhenRatioIsZero(t *testing.T) {
	sel, holders := leverageFixture(t, 5_000_000)
	sel.Tick(context.Background())

	if got := armedStrategy(holders["AAAA"]); got != config.StrategyBNFReversion {
		t.Fatalf("レバ上限が無い構成で arm されない: %v", got)
	}
	if s := sel.LeverageState(); s != LeverageOff {
		t.Fatalf("state=%q (want %q)", s, LeverageOff)
	}
}
