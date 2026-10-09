package app

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/strategy"
)

// quietSeries は**どのスクリーナーも発火しない**平坦な系列。
// 「その日は誰も引っかからなかった」という、研究モードで普通に起きる状態を作る。
func quietSeries() []market.Candle {
	base := time.Date(2025, 1, 6, 0, 0, 0, 0, clock.JST)
	cs := make([]market.Candle, strategy.DailyBarsRequired+2)
	for i := range cs {
		p := 1000.0
		cs[i] = market.Candle{
			OpenTime: base.AddDate(0, 0, i),
			Open:     p, High: p, Low: p, Close: p, Volume: 1000,
		}
	}
	return cs
}

// 🛑 **決定論経路(BuildConfig あり)で回す**。この穴は「ArmTemplate.Build が
// Triggered を見ない」ことで初めて害になるので、LLM 経路のフィクスチャで書くと
// 枝を復活させてもテストが緑のままになる(実際に変異テストで確認した)。
func quietLoop(t *testing.T, at time.Time, armed *[]*config.StrategyConfig) *AdvisorLoop {
	t.Helper()
	repo := repository.NewInMemoryCandleRepo()
	if err := repo.Upsert(context.Background(), "7203", quietSeries()); err != nil {
		t.Fatal(err)
	}
	l := armLoop(repo, []string{"7203"}, &llmCallCounter{}, armed, at, 10, 10)
	l.Screeners = []strategy.Screener{strategy.BNFReversion{}}
	l.BuildConfig = func(sym string, slot config.StrategyName, daily []market.Candle) (*config.StrategyConfig, error) {
		c := &config.StrategyConfig{
			ConfigID: "det-" + sym + "-" + string(slot), Symbol: sym,
			StrategyName: slot, Mode: config.ModePaper, HoldingMode: config.HoldingMultiday,
		}
		c.Entry.Direction = config.DirectionBuyOnly
		c.Risk.Quantity = 100
		return c, nil
	}
	return l
}

// 🚨 **決定論 arm にした瞬間、この 2 行の意味が裏返っていた**。
//
//	picks := selectAdvisePicks(..., nil)
//	if len(picks) == 0 { picks = ranked[:1] }
//
// LLM 経路では無害だった —— 何も発火していない日に最上位銘柄を LLM へ渡し、
// LLM が `no_trade` と答えて記録が残る、という**日記のための**枝だったから。
// 決定論 arm は `no_trade` を答えない。`ArmTemplate.Build` は `Triggered` を見ずに
// **必ず生きた config を作る**ので、この枝は「**発火していない銘柄を毎日 1 つ arm する**」
// に変わっていた。
//
// 決定論 arm の「トリガー成立 → 即 arm / 採用率 100%」と正面から食い違い、
// 「発火した候補だけを測る」という母集団の定義そのものを壊す。
func TestQuietDayArmsNothingInsteadOfTheTopRankedNonTrigger(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	var armed []*config.StrategyConfig
	l := quietLoop(t, inSession, &armed)
	l.Tick(context.Background())

	if len(armed) != 0 {
		t.Fatalf("発火ゼロの日に %d 件 arm した(発火していない銘柄が母集団に混じる): %+v",
			len(armed), armed)
	}
}

// 🛑 発火ゼロで arm ゼロは**正常**。これを「advisor が壊れている」と鳴らすと、
// 静かな日のたびに Error が飛んで人間が警報を無視するようになる。
// 警報は「候補はあったのに 1 つも arm できなかった」に限る。
func TestNoArmAlarmIgnoresDaysWithNoCandidates(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	var armed []*config.StrategyConfig
	l := quietLoop(t, inSession, &armed)
	n := &countingNotifier{}
	l.Notifier = n

	for i := 0; i < failStreakAlertThreshold+2; i++ {
		l.advise(context.Background(), inSession.Add(time.Duration(i)*time.Minute),
			map[string][]market.Candle{"7203": quietSeries()},
			[]strategy.Candidate{{Symbol: "7203", Strategy: config.StrategyBNFReversion, Triggered: false}},
			"heartbeat")
	}
	if n.count > 0 {
		t.Fatalf("候補ゼロの日に警報が %d 回鳴った — 静かな日のたびに Error が飛ぶ", n.count)
	}
	if l.failStreak != 0 {
		t.Errorf("failStreak = %d, want 0(候補が無い round は証拠にならない)", l.failStreak)
	}
}

type countingNotifier struct{ count int }

func (n *countingNotifier) Notify(_ context.Context, _, _, _ string) error {
	n.count++
	return nil
}

// 🚨 heartbeat は **1分**と事前登録した。ところが
// スキャン周期も 60 秒で、判定が `now.Sub(lastAdviseAt) >= hb` の素の比較だったため、
// ティック配送のゆらぎで**半分のティックが 60 秒をわずかに下回って落ちる** —
// 実効周期は約 90 秒だった。台帳に登録した数字と実際の挙動がずれる。
func TestHeartbeatFiresOnEveryTickWhenTheIntervalEqualsTheScanPeriod(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	var armed []*config.StrategyConfig
	l := quietLoop(t, inSession, &armed)
	l.Heartbeat = time.Minute
	l.advisedDay = inSession.Format("2006-01-02") // その日の初回ラウンドは済んでいる扱い
	l.lastAdviseAt = inSession

	// 60秒ちょうどより **わずかに手前**(配送ゆらぎ)。ここで落ちると次は約120秒後。
	justUnder := inSession.Add(time.Minute - 30*time.Millisecond)
	if got := l.shouldAdvise(justUnder, nil); got != "heartbeat" {
		t.Fatalf("59.97秒で見送った(reason=%q)— 実効周期が約90秒になり、"+
			"事前登録した『1分』と実際の建玉時刻分布がずれる", got)
	}
}

// 逆に、周期の半分も経っていないティックでは鳴らない(許容差が広すぎると
// heartbeat が「毎ティック」に化けて interval_min の意味が消える)。
func TestHeartbeatStillWaitsWellInsideTheInterval(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	var armed []*config.StrategyConfig
	l := quietLoop(t, inSession, &armed)
	l.Heartbeat = 10 * time.Minute
	l.advisedDay = inSession.Format("2006-01-02")
	l.lastAdviseAt = inSession

	if got := l.shouldAdvise(inSession.Add(time.Minute), nil); got != "" {
		t.Fatalf("10分周期なのに1分で鳴った: %q", got)
	}
}

// 🚨 「ランキングは日中ほぼ不変で、**変わるのは picked 列だけ**」を根拠に
// 書込周期を分離した。ところが分離したことで、**その唯一変わる列**が 1日約270ラウンド中
// 6 回しかサンプルされなくなった。日次総評パケットは
// `count(DISTINCT symbol) FILTER (WHERE picked)` を「何枠取れたか」として読むので、
// この数字が実際の arm 数を大幅に下回る。
//
// 周期に加えて **picked 集合が変わったときにも書く**。
// 変化は稀なので書込量は増えない。
func TestSnapshotAlsoWritesWhenThePickedSetChanges(t *testing.T) {
	inSession := time.Date(2026, 7, 21, 10, 0, 0, 0, clock.JST)
	var armed []*config.StrategyConfig
	l := quietLoop(t, inSession, &armed)
	l.SnapshotInterval = time.Hour

	first := []strategy.Candidate{{Symbol: "7203", Strategy: config.StrategyBNFReversion}}
	second := []strategy.Candidate{{Symbol: "6758", Strategy: config.StrategyBNFReversion}}

	if !l.shouldSnapshot(inSession, first) {
		t.Fatal("その日の初回は必ず書く")
	}
	l.markSnapshot(inSession, first)

	// 周期の内側でも、picked が変われば書く。
	if !l.shouldSnapshot(inSession.Add(time.Minute), second) {
		t.Fatal("picked が変わったのに書かない — 「何枠取れたか」が実際より小さく出る")
	}
	l.markSnapshot(inSession.Add(time.Minute), second)

	// picked が同じなら周期まで書かない(432,000行/日 を防ぐのが元の目的)。
	if l.shouldSnapshot(inSession.Add(2*time.Minute), second) {
		t.Fatal("picked が同じなのに毎ラウンド書いている — 書込量の分離が消えている")
	}
}

// 🚨 「picked が変わったら書く」を足したとき、**全ランキングを書き直していた**。
// `ranked` は日足由来で日中不変なので picked を
// 動かすのは**建玉の出入りだけ**だが、それは 1 日に数十回起きる。1 回 2,400 行
// (200銘柄 × 12スクリーナー)なので、当初の見立て「変化は稀なので書込量は増えない」
// は誤りで、実際は 1 日 19 万行規模 —— 周期を分離した目的そのものを潰していた。
//
// 周期のときは全ランキング、picked 変化のときは**差分だけ**。
func TestPickedChangeWritesOnlyTheDelta(t *testing.T) {
	prev := pickedSet([]strategy.Candidate{
		{Symbol: "7203", Strategy: config.StrategyBNFReversion},
		{Symbol: "6758", Strategy: config.StrategyBNFReversion},
	})
	now := []strategy.Candidate{
		{Symbol: "6758", Strategy: config.StrategyBNFReversion}, // 続投
		{Symbol: "8306", Strategy: config.StrategyBNFReversion}, // 新規
	}
	got := newlyPicked(prev, now)
	if len(got) != 1 || got[0].Symbol != "8306" {
		t.Fatalf("差分 = %+v, want 8306 の 1 件のみ — 全ランキングを書き直すと "+
			"周期分離の目的(書込量の抑制)が消える", got)
	}
	// 変化ゼロなら差分もゼロ。
	if n := len(newlyPicked(prev, []strategy.Candidate{
		{Symbol: "7203", Strategy: config.StrategyBNFReversion},
		{Symbol: "6758", Strategy: config.StrategyBNFReversion},
	})); n != 0 {
		t.Fatalf("変化ゼロなのに %d 件書こうとしている", n)
	}
}

// 🛑 警報メッセージは**数えているものをそのまま言う**。候補ゼロのラウンドは数えも
// リセットもしないので、実時間で連続しているとは限らない(朝の 2 連に午後の 1 件が
// 足されて「3 ティック連続」と鳴るのは嘘)。
func TestNoArmAlarmMessageDescribesWhatItActuallyCounts(t *testing.T) {
	src := mustReadDashboardSource(t)
	if strings.Contains(src, "consecutive ticks") {
		t.Error("警報が『consecutive ticks』と言っている — 候補ゼロのラウンドを飛ばして" +
			"数えているので実時間では連続していない")
	}
	if !strings.Contains(src, "consecutive rounds that had candidates") {
		t.Error("何を数えているかがメッセージから読めない")
	}
}

func mustReadDashboardSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("advisor_loop.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
