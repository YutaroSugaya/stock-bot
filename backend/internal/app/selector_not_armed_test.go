package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// 🚨 live がこのテストの理由: paper が建てた銘柄を live の selector が
// arm しない朝があった。原因は計画損失の上限(例: 35,000 < 49,514)だったが、selector の
// 篩(計画損失・1 単元の金額・資金枠・手動停止)はどれも**ログにも signal_rejections にも
// 何も残さず** `continue` していたので、特定に日足から ATR を手計算する羽目になった。
//
// 発火した候補を arm しなかったときは、理由を 1 行のログと signal_rejections の 1 行で残す。
// 連続する同じ (銘柄, 理由) は書かない(発注前ゲートの recordOn と同じエッジ記録)。

var fixedNow = time.Date(2026, 10, 7, 9, 0, 0, 0, time.FixedZone("JST", 9*3600))

func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestSelector_RecordsWhyTriggeredCandidateWasNotArmed_RiskPerTrade(t *testing.T) {
	sel, holders := riskPerTradeFixture(t, map[string]float64{"HEAVY": 450}) // 45,000 > 35,000
	sel.WithMaxRiskPerTradeJPY(35000)
	rej := repository.NewInMemoryRejectionRepo()
	logger, buf := captureLogger()
	sel.logger = logger
	sel.WithRejectionSink(rej, func() time.Time { return fixedNow })

	sel.Tick(context.Background())

	if got := armedStrategy(holders["HEAVY"]); got != config.StrategyNoTrade {
		t.Fatalf("前提: 計画損失 45,000 > 35,000 は arm されない: %v", got)
	}
	rows := rej.All()
	if len(rows) != 1 {
		t.Fatalf("signal_rejections に 1 行残るべき: %+v", rows)
	}
	r := rows[0]
	if r.Symbol != "HEAVY" || r.Reason != "selector_risk_per_trade" || r.ConfigID != "armed_HEAVY" || !r.CreatedAt.Equal(fixedNow) {
		t.Fatalf("行の中身: %+v", r)
	}
	if !strings.Contains(r.Detail, "45000") || !strings.Contains(r.Detail, "35000") {
		t.Fatalf("detail に計画損失と上限が無い: %q", r.Detail)
	}
	if out := buf.String(); !strings.Contains(out, "HEAVY") || !strings.Contains(out, "risk_per_trade") {
		t.Fatalf("ログに銘柄と理由が無い: %s", out)
	}
}

// 連続する同じ (銘柄, 理由) は 1 行だけ。30 分おきの Tick で 1 日 15 行積まない。
func TestSelector_NotArmedIsRecordedOnceWhileReasonUnchanged(t *testing.T) {
	sel, _ := riskPerTradeFixture(t, map[string]float64{"HEAVY": 450})
	sel.WithMaxRiskPerTradeJPY(35000)
	rej := repository.NewInMemoryRejectionRepo()
	sel.WithRejectionSink(rej, func() time.Time { return fixedNow })

	sel.Tick(context.Background())
	sel.Tick(context.Background())
	sel.Tick(context.Background())

	if n := len(rej.All()); n != 1 {
		t.Fatalf("同じ理由の連続は 1 行のはず: %d 行", n)
	}
}

// 理由が変わったら(上限を上げて通るようになり、その後また落ちる)新しいエッジとして書く。
func TestSelector_NotArmedRecordsAgainAfterTheCandidateWasArmed(t *testing.T) {
	sel, holders := riskPerTradeFixture(t, map[string]float64{"HEAVY": 450})
	sel.WithMaxRiskPerTradeJPY(35000)
	rej := repository.NewInMemoryRejectionRepo()
	sel.WithRejectionSink(rej, func() time.Time { return fixedNow })

	sel.Tick(context.Background()) // 落ちる(1 行目)
	sel.WithMaxRiskPerTradeJPY(100000)
	sel.Tick(context.Background()) // 通る
	if got := armedStrategy(holders["HEAVY"]); got != config.StrategyBNFReversion {
		t.Fatalf("上限 100,000 なら arm されるべき: %v", got)
	}
	sel.WithMaxRiskPerTradeJPY(35000)
	sel.Tick(context.Background()) // また落ちる(2 行目)

	if n := len(rej.All()); n != 2 {
		t.Fatalf("arm を挟んだら新しいエッジとして 2 行のはず: %d 行", n)
	}
}

func TestSelector_RecordsWhyTriggeredCandidateWasNotArmed_GrossNotionalCap(t *testing.T) {
	// 保証金 560,000 × 2.0 = 1,120,000 / 建玉 1,100,000 → 余力 20,000 < 1 単元 200,000。
	sel, _ := leverageFixture(t, 1_100_000)
	sel.WithLeverageHeadroom(stubMargin{equity: 560000}, 2.0)
	rej := repository.NewInMemoryRejectionRepo()
	sel.WithRejectionSink(rej, func() time.Time { return fixedNow })

	sel.Tick(context.Background())

	rows := rej.All()
	if len(rows) != 1 || rows[0].Symbol != "AAAA" || rows[0].Reason != "selector_gross_notional_cap" {
		t.Fatalf("資金枠で落ちた候補が記録されていない: %+v", rows)
	}
	if !strings.Contains(rows[0].Detail, "20000") {
		t.Fatalf("detail に余力が無い: %q", rows[0].Detail)
	}
}

func TestSelector_RecordsWhyTriggeredCandidateWasNotArmed_ManualSymbolBlock(t *testing.T) {
	sel, _ := riskPerTradeFixture(t, map[string]float64{"STOPPED": 100})
	sel.WithSymbolBlocks(&selBlocks{blocks: []port.SymbolBlock{{Symbol: "STOPPED"}}})
	rej := repository.NewInMemoryRejectionRepo()
	sel.WithRejectionSink(rej, func() time.Time { return fixedNow })

	sel.Tick(context.Background())

	rows := rej.All()
	if len(rows) != 1 || rows[0].Symbol != "STOPPED" || rows[0].Reason != "selector_manual_symbol_block" {
		t.Fatalf("手動停止で落ちた候補が記録されていない: %+v", rows)
	}
}

// 口座側の理由(枠が満杯)で 1 本も arm しない Tick は、銘柄ごとではなく 1 行で残す。
func TestSelector_LogsWhenArmingIsSkippedForAccountReasons(t *testing.T) {
	ctx := context.Background()
	candles := repository.NewInMemoryCandleRepo()
	if err := candles.Upsert(ctx, "AAAA", calmSeries(30)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	holders := map[string]*ActiveConfigHolder{"AAAA": NewActiveConfigHolder(noTradeCfgFor("AAAA"))}
	pos := repository.NewInMemoryPositionRepo()
	if _, err := pos.Insert(ctx, port.PositionInsertInput{
		Symbol: "ZZZZ", Side: order.SideBuy, Quantity: 100, EntryPrice: 1000,
		StrategyConfigID: "seed", HoldingMode: order.HoldingMultiday,
	}); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	armCfg := func(sym string) *config.StrategyConfig {
		c := &config.StrategyConfig{ConfigID: "armed_" + sym, Symbol: sym,
			StrategyName: config.StrategyBNFReversion, HoldingMode: config.HoldingMultiday}
		c.Risk.Quantity = 100
		return c
	}
	logger, buf := captureLogger()
	sel := NewSelector(candles, pos, holders, []strategy.Screener{plannedStopScreener{}}, 1, 0,
		armCfg, noTradeCfgFor, nil, logger) // accountMax 1 = 建玉 1 で満杯

	sel.Tick(ctx)

	if got := armedStrategy(holders["AAAA"]); got != config.StrategyNoTrade {
		t.Fatalf("前提: 枠が満杯なので arm されない: %v", got)
	}
	out := buf.String()
	if !strings.Contains(out, "account_full") || !strings.Contains(out, "open_count=1") {
		t.Fatalf("口座側の見送り理由がログに無い: %s", out)
	}
}

// 挿していない(research)なら従来どおり何も書かない。
func TestSelector_NoRejectionSinkMeansNoRows(t *testing.T) {
	sel, _ := riskPerTradeFixture(t, map[string]float64{"HEAVY": 450})
	sel.WithMaxRiskPerTradeJPY(35000)
	sel.Tick(context.Background()) // panic しない・何も書かない
}
