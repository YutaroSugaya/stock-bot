package app

import (
	"context"
	"testing"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/testutil"
)

// selector は **枠を配る前に** 1本あたりの計画損失で候補を落とす。
//
// 🚨 発注前ゲート(risk の `risk_per_trade`)だけでは足りない。selector は毎 Tick
// 同じ日足から決定論的に同じ首位を arm するので、「arm 済み・発注は毎回 reject」の
// 銘柄が口座の建玉枠を**永久に**占有する(8309 が分割未調整で起こした
// 「発注待ちのまま動かない沈黙のデッドロック」と同じ形)。事前登録が貸借の
// 売り候補について「枠を配る前にも落とす」と定めているのと同じ理由。

// plannedStopScreener は計画 SL を銘柄ごとに固定して返すスタブ。
// 実 screener の ATR 計算に依存せず、境界(上限ちょうど / 未算出)を厳密に突く。
type plannedStopScreener struct {
	stopJPY map[string]float64 // symbol -> 計画ストップ幅(円/株)。欠けていれば 0 = 未算出
}

func (plannedStopScreener) Name() config.StrategyName { return config.StrategyBNFReversion }

func (s plannedStopScreener) Screen(symbol string, d []market.Candle) strategy.Candidate {
	return strategy.Candidate{
		Symbol:      symbol,
		Strategy:    config.StrategyBNFReversion,
		Triggered:   true,
		Score:       1.0,
		StopLossJPY: s.stopJPY[symbol],
	}
}

// 全銘柄が同じ値段・同じ株数で、違うのは計画 SL だけ。
func riskPerTradeFixture(t *testing.T, stops map[string]float64) (*Selector, map[string]*ActiveConfigHolder) {
	t.Helper()
	candles := repository.NewInMemoryCandleRepo()
	holders := map[string]*ActiveConfigHolder{}
	for sym := range stops {
		if err := candles.Upsert(context.Background(), sym, calmSeries(30)); err != nil {
			t.Fatalf("upsert %s: %v", sym, err)
		}
		holders[sym] = NewActiveConfigHolder(noTradeCfgFor(sym))
	}
	armCfg := func(sym string) *config.StrategyConfig {
		c := &config.StrategyConfig{ConfigID: "armed_" + sym, Symbol: sym,
			StrategyName: config.StrategyBNFReversion, HoldingMode: config.HoldingMultiday}
		c.Risk.Quantity = 100
		return c
	}
	sel := NewSelector(candles, repository.NewInMemoryPositionRepo(), holders,
		[]strategy.Screener{plannedStopScreener{stopJPY: stops}}, len(stops), 0,
		armCfg, noTradeCfgFor, nil, testutil.SilentLogger())
	return sel, holders
}

func armedStrategy(h *ActiveConfigHolder) config.StrategyName {
	c := h.Get()
	if c == nil {
		return config.StrategyNoTrade
	}
	return c.StrategyName
}

func TestSelector_SkipsCandidatesOverRiskPerTradeCap(t *testing.T) {
	// LIGHT: 250 × 100 = 25,000 / HEAVY: 450 × 100 = 45,000
	sel, holders := riskPerTradeFixture(t, map[string]float64{"LIGHT": 250, "HEAVY": 450})
	sel.WithMaxRiskPerTradeJPY(35000)
	sel.Tick(context.Background())

	if got := armedStrategy(holders["LIGHT"]); got != config.StrategyBNFReversion {
		t.Fatalf("計画損失 25,000 ≤ 35,000 は arm されるべき: %v", got)
	}
	if got := armedStrategy(holders["HEAVY"]); got != config.StrategyNoTrade {
		t.Fatalf("計画損失 45,000 > 35,000 を arm した(枠を食う): %v", got)
	}
}

// 🛑 上限ちょうどは通す(発注前ゲートと同じ境界。ここだけ `>=` にすると
// 「arm されたのに必ず reject」または「arm されないのにゲートは通す」がズレて出る)。
func TestSelector_ArmsCandidateExactlyAtRiskPerTradeCap(t *testing.T) {
	sel, holders := riskPerTradeFixture(t, map[string]float64{"EXACT": 350}) // 35,000 ちょうど
	sel.WithMaxRiskPerTradeJPY(35000)
	sel.Tick(context.Background())

	if got := armedStrategy(holders["EXACT"]); got != config.StrategyBNFReversion {
		t.Fatalf("上限ちょうどを落とした: %v", got)
	}
}

// cap 0 = 無効。research / harvest の標本をこの理由で censoring しない。
func TestSelector_RiskPerTradeCapZeroDisablesFilter(t *testing.T) {
	sel, holders := riskPerTradeFixture(t, map[string]float64{"HEAVY": 5000})
	sel.Tick(context.Background()) // WithMaxRiskPerTradeJPY を挿さない = 既定 0

	if got := armedStrategy(holders["HEAVY"]); got != config.StrategyBNFReversion {
		t.Fatalf("上限未設定なのに落とした: %v", got)
	}
}

// 🛑 **計画 SL を報告しない候補(0)は素通しする。** 0 は「算出できなかった」であって
// 「損失ゼロ」ではないが、ここで fail-close にすると出口を config から読む戦略が
// 黙って永久に arm されなくなる。重さの最終判定は発注前ゲートが持つ。
func TestSelector_ArmsCandidateWithUnknownPlannedStop(t *testing.T) {
	sel, holders := riskPerTradeFixture(t, map[string]float64{"UNKNOWN": 0})
	sel.WithMaxRiskPerTradeJPY(1)
	sel.Tick(context.Background())

	if got := armedStrategy(holders["UNKNOWN"]); got != config.StrategyBNFReversion {
		t.Fatalf("計画 SL 未算出の候補を落とした(発注前ゲートに委ねるべき): %v", got)
	}
}

// 上限で落ちた候補は**枠を消費しない** — 下位の軽い候補が繰り上がる。
// これが無いと「重い首位が枠を握って誰も建たない」元の壊れ方に戻る。
func TestSelector_HeavyCandidateDoesNotConsumeSlot(t *testing.T) {
	sel, holders := riskPerTradeFixture(t, map[string]float64{"HEAVY": 450, "LIGHT": 250})
	sel.accountMax = 1 // 枠は 1 つだけ
	sel.WithMaxRiskPerTradeJPY(35000)
	sel.Tick(context.Background())

	if got := armedStrategy(holders["LIGHT"]); got != config.StrategyBNFReversion {
		t.Fatalf("重い候補が枠を握ったまま、軽い候補が繰り上がらなかった: %v", got)
	}
}
