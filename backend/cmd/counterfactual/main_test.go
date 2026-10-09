package main

import (
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

func d(day int) time.Time { return time.Date(2026, 8, day, 0, 0, 0, 0, clock.JST) }

func bars(days ...market.Candle) []market.Candle { return days }

func candle(day int, o, h, l, c float64) market.Candle {
	return market.Candle{Symbol: "7203", Interval: 24 * time.Hour, OpenTime: d(day), Open: o, High: h, Low: l, Close: c}
}

func pos(id int64, strategy string, tp, sl float64, closedDay int, closePrice float64) port.ClosedPositionSnapshot {
	return port.ClosedPositionSnapshot{
		PositionID: id, Symbol: "7203", Strategy: strategy, Side: string(order.SideBuy), Quantity: 100,
		EntryPrice: 1000, TakeProfitPrice: tp, StopLossPrice: sl, ClosePrice: closePrice,
		NetJPY: 500, CloseReason: "manual", ClosedAt: time.Date(2026, 8, closedDay, 15, 0, 0, 0, clock.JST),
	}
}

// 🚨 締め当日に走らせると**決済後の日足が 1 本も無い**(当日の日足は翌朝の fetch-daily
// 待ち)。それを「未決着 = 最終終値で評価」と混ぜると、締め当日の建玉が全部
// 「含み 0 円」として集計に足し込まれる。材料無しは損益に寄与させない。
func TestAnalyse_NoBarsAfterCloseIsNotUnresolved(t *testing.T) {
	rep := analyse([]port.ClosedPositionSnapshot{pos(1, "abs_momentum_v2", 1100, 900, 14, 1000)},
		func(string) ([]market.Candle, error) { return bars(candle(14, 1000, 1010, 990, 1000)), nil })

	s := rep.ByStrategy[0]
	if s.NoData != 1 || s.Unresolved != 0 {
		t.Fatalf("材料無しが未決着に化けている: %+v", s)
	}
	if s.UnrealisedJPY != 0 || s.UnrealisedN != 0 {
		t.Fatalf("材料が無いのに含みを計上している: %+v", s)
	}
	if rep.Rows[0].Outcome != "no_data" {
		t.Fatalf("outcome = %q", rep.Rows[0].Outcome)
	}
}

// 決着(バリア到達)と含み(未決着の mark-to-market)は**別の列**。足すと、全銘柄が
// 同じ日で切られた 1 回のドロー(戦略間で完全に相関)が「反実仮想の損益」に化ける。
func TestAnalyse_SettledAndUnrealisedAreSeparate(t *testing.T) {
	rep := analyse([]port.ClosedPositionSnapshot{
		pos(1, "s", 1100, 900, 14, 1000), // 決着(TP)
		pos(2, "s", 1100, 900, 14, 1000), // 未決着
	}, func(string) ([]market.Candle, error) {
		return bars(candle(14, 1000, 1010, 990, 1000), candle(17, 1050, 1150, 1040, 1100)), nil
	})
	// 1本目は TP 到達、2本目も同じバーを見るので TP になる — 分けるため 2 本目だけ TP を遠くする。
	rep2 := analyse([]port.ClosedPositionSnapshot{pos(2, "s", 9999, 900, 14, 1000)},
		func(string) ([]market.Candle, error) {
			return bars(candle(14, 1000, 1010, 990, 1000), candle(17, 1050, 1150, 1040, 1100)), nil
		})

	s := rep.ByStrategy[0]
	if s.SettledN != 2 || s.UnrealisedN != 0 {
		t.Fatalf("決着の勘定 = %+v", s)
	}
	u := rep2.ByStrategy[0]
	if u.UnrealisedN != 1 || u.SettledN != 0 {
		t.Fatalf("未決着の勘定 = %+v", u)
	}
	if u.UnrealisedJPY != 10000 { // 終値 1100 − 建値 1000 = +100 × 100株
		t.Fatalf("含み = %v, want 10000", u.UnrealisedJPY)
	}
}

// ambiguous は N に数えるが損益には寄与しない。
func TestAnalyse_AmbiguousContributesNoPnL(t *testing.T) {
	rep := analyse([]port.ClosedPositionSnapshot{pos(1, "s", 1100, 900, 14, 1000)},
		func(string) ([]market.Candle, error) {
			return bars(candle(14, 1000, 1010, 990, 1000), candle(17, 1000, 1150, 850, 1000)), nil
		})
	s := rep.ByStrategy[0]
	if s.Ambiguous != 1 || s.SettledJPY != 0 || s.UnrealisedJPY != 0 {
		t.Fatalf("ambiguous の勘定 = %+v", s)
	}
	if s.N != 1 {
		t.Fatalf("N には数える: %+v", s)
	}
}

// 🛑 窓を開けてバリアを飛び越えた日は、実際の約定は**寄り値**。バリア価格をそのまま
// 使うと系統的にずれる(実データでは決着した建玉の 1/3 程度が該当し、合計で無視できない差になる)。
func TestAnalyse_GapThroughBarrierFillsAtOpen(t *testing.T) {
	// 寄り 1200 で TP 1100 を飛び越え → 有利側(1200)で約定。
	tp := analyse([]port.ClosedPositionSnapshot{pos(1, "s", 1100, 900, 14, 1000)},
		func(string) ([]market.Candle, error) {
			return bars(candle(14, 1000, 1010, 990, 1000), candle(17, 1200, 1250, 1180, 1240)), nil
		})
	if got := tp.Rows[0].SettledJPY; got != 20000 {
		t.Fatalf("TP 飛び越え = %v, want 20000(寄り 1200 で約定)", got)
	}
	// 寄り 800 で SL 900 を割って始まった → 不利側(800)で約定。
	sl := analyse([]port.ClosedPositionSnapshot{pos(1, "s", 1100, 900, 14, 1000)},
		func(string) ([]market.Candle, error) {
			return bars(candle(14, 1000, 1010, 990, 1000), candle(17, 800, 850, 780, 820)), nil
		})
	if got := sl.Rows[0].SettledJPY; got != -20000 {
		t.Fatalf("SL 飛び越え = %v, want -20000(寄り 800 で約定)", got)
	}
}

// サイクル境界を跨いだ標本は出口幾何が別物。黙って 1 つの表に並べない。
func TestAnalyse_WarnsWhenSpanningCycleBoundary(t *testing.T) {
	rep := analyse([]port.ClosedPositionSnapshot{
		pos(1, "s", 1100, 900, 7, 1000),  // 境界の前
		pos(2, "s", 1100, 900, 13, 1000), // 境界の後
	}, func(string) ([]market.Candle, error) { return bars(candle(14, 1000, 1010, 990, 1000)), nil })
	if len(rep.Warnings) == 0 {
		t.Fatal("サイクル境界を跨いだのに警告が無い")
	}
	if rep.FirstClose != "2026-08-07" || rep.LastClose != "2026-08-13" {
		t.Fatalf("対象期間が出ていない: %+v", rep)
	}
}

// 分割の連鎖調整で過去バーが新スケールに書き換わると、凍結価格(旧スケール)との比較が
// 壊れて「初日に即 SL」と誤判定する。決済当日の日足と実約定値が桁違いなら推定しない。
func TestAnalyse_SkipsSplitScaleMismatch(t *testing.T) {
	p := pos(1, "s", 1100, 900, 14, 5000) // 実約定 5000 に対し当日バーは 1000 = 5倍
	rep := analyse([]port.ClosedPositionSnapshot{p},
		func(string) ([]market.Candle, error) {
			return bars(candle(14, 1000, 1010, 990, 1000), candle(17, 900, 950, 850, 880)), nil
		})
	if rep.ByStrategy[0].Skipped != 1 || len(rep.Rows) != 0 {
		t.Fatalf("分割疑いを推定に混ぜている: %+v", rep.ByStrategy[0])
	}
	if len(rep.Warnings) == 0 {
		t.Fatal("除外したことが見えない")
	}
}

// 日足が無い銘柄は推定しない。**戦略別に**何本落ちたかが見えること。
func TestAnalyse_SkipsMissingDailyPerStrategy(t *testing.T) {
	rep := analyse([]port.ClosedPositionSnapshot{pos(1, "s", 1100, 900, 14, 1000)},
		func(string) ([]market.Candle, error) { return nil, errors.New("no csv") })
	s := rep.ByStrategy[0]
	if s.Skipped != 1 || s.N != 1 || len(rep.Rows) != 0 {
		t.Fatalf("欠測の勘定 = %+v", s)
	}
}

// トレール建玉の実際の出口は ratchet 線。固定 TP/SL で歩いた推定であることを行に明記する。
func TestAnalyse_FlagsRatchetPositions(t *testing.T) {
	p := pos(1, "bnf_reversion_trail", 0, 900, 14, 1000)
	p.RatchetArmJPY = 50
	rep := analyse([]port.ClosedPositionSnapshot{p},
		func(string) ([]market.Candle, error) {
			return bars(candle(14, 1000, 1010, 990, 1000), candle(17, 1000, 1050, 950, 1020)), nil
		})
	if rep.Rows[0].Note == "" || !contains(rep.Rows[0].Note, "トレール") {
		t.Fatalf("トレール建玉に注記が無い: %q", rep.Rows[0].Note)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
