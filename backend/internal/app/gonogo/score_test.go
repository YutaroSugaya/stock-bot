package gonogo

import (
	"fmt"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

func judged(date, sym, verdict string) port.GoNoGoRecord {
	r := port.GoNoGoRecord{Date: date, Symbol: sym, Status: port.GoNoGoStatusOK, PromptVersion: "gonogo-v1"}
	r.Verdict = verdict
	return r
}

func trade(sym, strat string, day int, net float64) ScoredTrade {
	return ScoredTrade{Symbol: sym, Strategy: strat, EntryPrice: 1000, Quantity: 100, NetJPY: net,
		OpenedAt: time.Date(2026, 10, day, 9, 30, 0, 0, clock.JST)}
}

// (日付, 銘柄)で判定と paper の bnf 家族の建玉(その日に建ったもの)を結合し、3 群 + 未判定で net を比べる。
func TestScore_JoinsByDateAndSymbol(t *testing.T) {
	judgments := []port.GoNoGoRecord{
		judged("2026-10-02", "6594", port.GoNoGoNoGo),
		judged("2026-10-02", "7203", port.GoNoGoGo),
		judged("2026-10-05", "7203", port.GoNoGoUnknown),
	}
	trades := []ScoredTrade{
		trade("6594", "bnf_day2_reversion", 2, -5000),
		trade("6594", "bnf_day2_reversion_trail", 2, -6000),
		trade("7203", "bnf_day2_reversion", 2, 3000),
		trade("7203", "bnf_day2_reversion", 5, 1000), // 10/5 は unknown
		trade("9984", "bnf_reversion", 2, 2000),      // 判定なし
		trade("1111", "post_jump_drift", 2, 9999),    // bnf 家族の外は数えない
	}
	rep, err := Score(judgments, trades, "", 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	g := map[string]ScoreGroup{}
	for _, x := range rep.Groups {
		g[x.Name] = x
	}
	if g[port.GoNoGoNoGo].N != 2 || g[port.GoNoGoGo].N != 1 || g[port.GoNoGoUnknown].N != 1 || g[GroupUnjudged].N != 1 {
		t.Fatalf("groups = %+v", rep.Groups)
	}
	// ¥1M 正規化: 建玉 10 万 → ×10。
	if g[port.GoNoGoNoGo].NetPer1MJPY != -110000 || g[port.GoNoGoGo].MeanPer1MJPY != 30000 {
		t.Fatalf("no_go = %+v go = %+v", g[port.GoNoGoNoGo], g[port.GoNoGoGo])
	}
	if g[port.GoNoGoNoGo].DayBlocks != 1 {
		t.Fatalf("day_blocks = %d", g[port.GoNoGoNoGo].DayBlocks)
	}
}

// 版をまたいだ判定は混ぜない(版を指定しなければエラー)。
func TestScore_RefusesMixedPromptVersions(t *testing.T) {
	a := judged("2026-10-02", "6594", port.GoNoGoNoGo)
	b := judged("2026-10-05", "7203", port.GoNoGoGo)
	b.PromptVersion = "gonogo-v2"
	if _, err := Score([]port.GoNoGoRecord{a, b}, nil, "", 10, 1); err == nil {
		t.Fatal("版の混在を通した")
	}
	rep, err := Score([]port.GoNoGoRecord{a, b}, nil, "gonogo-v2", 10, 1)
	if err != nil || rep.PromptVersion != "gonogo-v2" {
		t.Fatalf("%+v %v", rep, err)
	}
}

// live で人間が止めた銘柄は、止めていた日の paper の結果と突き合わせる(止めた判断の答え合わせ)。
func TestBlockedDays_FromOperationLog(t *testing.T) {
	ops := []BlockOp{
		{At: time.Date(2026, 10, 2, 8, 30, 0, 0, clock.JST), Symbol: "6594", Action: "block"},
		{At: time.Date(2026, 10, 6, 12, 0, 0, 0, clock.JST), Symbol: "6594", Action: "release"},
	}
	trades := []ScoredTrade{
		trade("6594", "bnf_day2_reversion", 2, -5000),
		trade("6594", "bnf_day2_reversion", 6, 1000), // 解除(12:00)より前に建った = 止めていた間
		trade("6594", "bnf_day2_reversion", 7, 7000), // 解除の後
	}
	rows := BlockedTrades(ops, trades, time.Date(2026, 10, 31, 0, 0, 0, 0, clock.JST))
	if len(rows) != 2 || rows[0].Trade.NetJPY != -5000 || rows[1].Trade.NetJPY != 1000 {
		t.Fatalf("rows = %+v", rows)
	}
}

func judgedAt(date, sym, verdict string, h, m int) port.GoNoGoRecord {
	r := judged(date, sym, verdict)
	d, _ := time.ParseInLocation("2006-01-02", date, clock.JST)
	r.JudgedAt = d.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	return r
}

// 建った後に下した判定は、建った時点で無かった情報を見ている。採点に使うのは建つ前の最後の判定だけで、
// 建った後の判定しか無い建玉は「後出し」の群に分ける(未判定にも混ぜない)。
func TestScore_UsesOnlyJudgmentsMadeBeforeEntry(t *testing.T) {
	judgments := []port.GoNoGoRecord{
		judgedAt("2026-10-02", "6594", port.GoNoGoGo, 8, 15),
		judgedAt("2026-10-02", "6594", port.GoNoGoUnknown, 8, 40), // 建つ前の最後 = これ
		judgedAt("2026-10-02", "6594", port.GoNoGoNoGo, 10, 0),    // 建った(9:30)後の再判定
		judgedAt("2026-10-02", "7203", port.GoNoGoNoGo, 11, 0),    // 建った後にしか無い
	}
	trades := []ScoredTrade{
		trade("6594", "bnf_day2_reversion", 2, -5000),
		trade("7203", "bnf_day2_reversion", 2, -3000),
	}
	rep, err := Score(judgments, trades, "", 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	g := map[string]ScoreGroup{}
	var names []string
	for _, x := range rep.Groups {
		g[x.Name] = x
		names = append(names, x.Name)
	}
	if g[port.GoNoGoUnknown].N != 1 || g[port.GoNoGoNoGo].N != 0 || g[GroupLate].N != 1 || g[GroupUnjudged].N != 0 {
		t.Fatalf("groups = %+v", rep.Groups)
	}
	want := []string{port.GoNoGoNoGo, port.GoNoGoUnknown, port.GoNoGoGo, GroupLate, GroupUnjudged}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("群の並び = %v", names)
	}
}

// no_go はカテゴリ(不正・希薄化・行政処分…)で割って読む。bnf は悪材料で売られた銘柄を買う戦略なので、
// 「悪材料がある」だけの no_go は母集団のほぼ全部に当たる。効くとすれば構造的な悪材料と一時的な悪材料の差で、
// それはカテゴリ別にしか見えない。
func TestScore_SplitsNoGoByCategory(t *testing.T) {
	withCat := func(date, sym, cat string) port.GoNoGoRecord {
		r := judged(date, sym, port.GoNoGoNoGo)
		r.Category = cat
		return r
	}
	judgments := []port.GoNoGoRecord{
		withCat("2026-10-02", "6594", "accounting_fraud"),
		withCat("2026-10-02", "6997", "administrative_action"),
		withCat("2026-10-05", "5076", "dilution"),
		withCat("2026-10-05", "4666", ""), // カテゴリ無し = "none"
		judged("2026-10-02", "7203", port.GoNoGoGo),
	}
	trades := []ScoredTrade{
		trade("6594", "bnf_reversion", 2, -5000),
		trade("6594", "bnf_reversion_trail", 2, -6000),
		trade("6997", "bnf_reversion", 2, 2000),
		trade("5076", "bnf_reversion", 5, 1000),
		trade("4666", "bnf_day2_reversion", 5, -1000),
		trade("7203", "bnf_reversion", 2, 3000), // go はカテゴリ表に出ない
	}
	rep, err := Score(judgments, trades, "", 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ScoreGroup{}
	var order []string
	for _, c := range rep.NoGoByCategory {
		got[c.Name] = c
		order = append(order, c.Name)
	}
	if len(order) != 4 || order[0] != "accounting_fraud" || order[3] != "none" {
		t.Fatalf("カテゴリは名前順・空は none: %v", order)
	}
	if got["accounting_fraud"].N != 2 || got["accounting_fraud"].NetJPY != -11000 {
		t.Fatalf("accounting_fraud = %+v", got["accounting_fraud"])
	}
	if got["administrative_action"].N != 1 || got["dilution"].N != 1 || got["none"].N != 1 {
		t.Fatalf("categories = %+v", rep.NoGoByCategory)
	}
	// 群の合計と一致する(カテゴリ表は no_go 群の内訳)。
	var sum int
	for _, c := range rep.NoGoByCategory {
		sum += c.N
	}
	for _, g := range rep.Groups {
		if g.Name == port.GoNoGoNoGo && g.N != sum {
			t.Fatalf("no_go N=%d だがカテゴリの合計 %d", g.N, sum)
		}
	}
}
