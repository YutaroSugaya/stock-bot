package gonogo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/gonogo"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

func bars(n int, close float64) []market.Candle {
	out := make([]market.Candle, n)
	for i := range out {
		d := time.Date(2026, 8, 1, 0, 0, 0, 0, clock.JST).AddDate(0, 0, i)
		out[i] = market.Candle{Symbol: "7203", Interval: 24 * time.Hour, OpenTime: d,
			Open: close, High: close + 10, Low: close - 10, Close: close, Volume: 1000}
	}
	return out
}

// 入力は決定論の材料(直近 30 本・25 日線乖離・出来高倍率)で、同じ入力なら同じハッシュ。
func TestBuildPacket_IsDeterministic(t *testing.T) {
	cs := bars(40, 2000)
	cs[len(cs)-1].Close = 1800
	cs[len(cs)-1].Volume = 3000
	p1, raw1, h1 := BuildPacket("2026-10-02", "7203", cs, []string{"b", "a"}, []string{"live"})
	_, raw2, h2 := BuildPacket("2026-10-02", "7203", cs, []string{"a", "b"}, []string{"live"})
	if h1 != h2 || string(raw1) != string(raw2) {
		t.Fatal("同じ入力でハッシュが変わる")
	}
	if len(p1.Bars) != 30 || p1.Bars[29].Close != 1800 {
		t.Fatalf("直近 30 本: %d", len(p1.Bars))
	}
	if p1.Dev25Pct > -9 || p1.Dev25Pct < -10 { // 25 日線 = (24×2000+1800)/25 = 1992 → −9.64%
		t.Fatalf("乖離 = %v", p1.Dev25Pct)
	}
	if p1.VolumeRatio25 != 2.78 { // 平均は当日を含む 25 本(bnf の avgVolume と同じ)= 3000 / 1080
		t.Fatalf("出来高倍率 = %v", p1.VolumeRatio25)
	}
}

func TestMergeTargets(t *testing.T) {
	paper := []strategy.Candidate{
		{Symbol: "7203", Strategy: config.StrategyBNFDay2Reversion},
		{Symbol: "7203", Strategy: config.StrategyBNFDay2ReversionTrail},
		{Symbol: "6594", Strategy: config.StrategyBNFReversion},
	}
	live := []strategy.Candidate{{Symbol: "7203", Strategy: config.StrategyBNFDay2ReversionTrail}}
	got := MergeTargets(paper, live)
	if len(got) != 2 || got[0].Symbol != "6594" || got[1].Symbol != "7203" {
		t.Fatalf("銘柄で 1 本にまとめる: %+v", got)
	}
	if len(got[1].Tracks) != 2 || len(got[1].Strategies) != 2 {
		t.Fatalf("7203 は paper と live の両方: %+v", got[1])
	}
}

func hoursTokyo() session.TradingHours {
	return session.TradingHours{TZ: clock.JST,
		Sessions:    []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:30"}},
		EntryCutoff: "14:55", ForceFlatAt: "14:50"}
}

func TestPreconditions(t *testing.T) {
	h := hoursTokyo()
	thu := time.Date(2026, 10, 1, 8, 15, 0, 0, clock.JST)
	ok := Preflight{Hours: h, Now: thu, UniverseModTime: thu.Add(-time.Hour), LatestBarDate: "2026-09-30"}
	if reason := ok.Check(true); reason != "" {
		t.Fatalf("満たしているのに止まった: %s", reason)
	}
	for name, p := range map[string]Preflight{
		"weekend":         {Hours: h, Now: time.Date(2026, 10, 3, 8, 15, 0, 0, clock.JST), UniverseModTime: thu, LatestBarDate: "2026-10-02"},
		"after_close":     {Hours: h, Now: thu.Add(7*time.Hour + 15*time.Minute), UniverseModTime: thu, LatestBarDate: "2026-09-30"},
		"universe_old":    {Hours: h, Now: thu, UniverseModTime: thu.AddDate(0, 0, -1), LatestBarDate: "2026-09-30"},
		"universe_absent": {Hours: h, Now: thu, LatestBarDate: "2026-09-30"},
		"daily_stale":     {Hours: h, Now: thu, UniverseModTime: thu, LatestBarDate: "2026-09-29"},
	} {
		if p.Check(true) == "" {
			t.Errorf("%s: 前提を満たさないのに走る", name)
		}
	}
	// 手動(銘柄指定)はユニバースを見ない。
	if reason := (Preflight{Hours: h, Now: thu, LatestBarDate: "2026-09-30"}).Check(false); reason != "" {
		t.Errorf("手動でユニバースを要求した: %s", reason)
	}
}

type fakeJudge struct {
	mu    sync.Mutex
	calls map[string]int
	fail  map[string]error
}

func (f *fakeJudge) Judge(_ context.Context, packet []byte) (port.GoNoGoJudgment, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sym := symbolOf(packet)
	f.calls[sym]++
	if err := f.fail[sym]; err != nil {
		return port.GoNoGoJudgment{}, "", err
	}
	return port.GoNoGoJudgment{Verdict: port.GoNoGoGo, Category: "none", Confidence: "mid", SummaryJA: "ok",
		Sources: []port.GoNoGoSource{}}, "m", nil
}

func newRunner(t *testing.T, f *fakeJudge) (*Runner, *gonogo.Journal) {
	j := &gonogo.Journal{Dir: t.TempDir()}
	return &Runner{Journal: j, Judge: f, Clock: clock.Fixed(time.Date(2026, 10, 2, 8, 15, 0, 0, clock.JST)),
		Concurrency: 2, Daily: func(string) []market.Candle { return bars(40, 2000) }}, j
}

func targets(syms ...string) []Target {
	var out []Target
	for _, s := range syms {
		out = append(out, Target{Symbol: s, Strategies: []string{"bnf_day2_reversion_trail"}, Tracks: []string{"live"}})
	}
	return out
}

// 2 回続けて起動しても二重に判定しない。失敗した銘柄だけ次の起動で判定し直す。
func TestRunner_IsIdempotentAndRetriesErrors(t *testing.T) {
	f := &fakeJudge{calls: map[string]int{}, fail: map[string]error{"6594": errors.New("network down")}}
	r, j := newRunner(t, f)
	sum, err := r.Run(context.Background(), "2026-10-02", targets("6594", "7203"))
	if err != nil || sum.Judged != 1 || sum.Errors != 1 {
		t.Fatalf("1 回目: %+v %v", sum, err)
	}
	delete(f.fail, "6594") // ネットワークが戻った
	sum, _ = r.Run(context.Background(), "2026-10-02", targets("6594", "7203"))
	if sum.Judged != 1 || sum.Skipped != 1 {
		t.Fatalf("2 回目は失敗した 6594 だけ: %+v", sum)
	}
	if f.calls["7203"] != 1 || f.calls["6594"] != 2 {
		t.Fatalf("二重に判定した: %v", f.calls)
	}
	latest, _ := j.ForDate(context.Background(), "2026-10-02")
	if latest["6594"].Status != port.GoNoGoStatusOK {
		t.Fatalf("判定し直した結果が正になっていない: %+v", latest["6594"])
	}
	if latest["6594"].PacketSHA256 == "" || latest["6594"].PromptVersion == "" || latest["6594"].Model != "m" {
		t.Fatalf("記録に入力のハッシュ・版・モデルが無い: %+v", latest["6594"])
	}
}

// 利用上限に当たったら、その実行の残りは error: usage_limit にして終わる。
func TestRunner_StopsOnUsageLimit(t *testing.T) {
	f := &fakeJudge{calls: map[string]int{}, fail: map[string]error{}}
	var syms []string
	for i := 0; i < 6; i++ {
		s := fmt.Sprintf("10%02d", i)
		syms = append(syms, s)
		f.fail[s] = fmt.Errorf("%w: limit", gonogo.ErrUsageLimit)
	}
	r, j := newRunner(t, f)
	r.Concurrency = 1
	sum, _ := r.Run(context.Background(), "2026-10-02", targets(syms...))
	total := 0
	for _, n := range f.calls {
		total += n
	}
	if total != 1 {
		t.Fatalf("上限の後も呼び続けた: %d 回", total)
	}
	latest, _ := j.ForDate(context.Background(), "2026-10-02")
	if len(latest) != 6 || sum.UsageLimited != 6 {
		t.Fatalf("残りが usage_limit で記録されていない: %d 件 %+v", len(latest), sum)
	}
	for _, rec := range latest {
		if rec.Status != port.GoNoGoStatusError || rec.Error == "" {
			t.Fatalf("%+v", rec)
		}
	}
}

// 実行のたびに .md を書き直す。
func TestRunner_WritesMarkdown(t *testing.T) {
	f := &fakeJudge{calls: map[string]int{}, fail: map[string]error{}}
	r, j := newRunner(t, f)
	if _, err := r.Run(context.Background(), "2026-10-02", targets("7203")); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Read("2026-10-02"); err != nil {
		t.Fatal(err)
	}
	if _, err := gonogoReadFile(j.MarkdownPath("2026-10-02")); err != nil {
		t.Fatalf(".md が無い: %v", err)
	}
}

// どの起動経路(launchd / catchup / 手動 / 画面のボタン)で判定したかを 1 行ごとに残す(後から振り返る)。
func TestRunner_RecordsTrigger(t *testing.T) {
	f := &fakeJudge{calls: map[string]int{}, fail: map[string]error{}}
	r, j := newRunner(t, f)
	r.Trigger = TriggerButton
	if _, err := r.Run(context.Background(), "2026-10-02", targets("7203")); err != nil {
		t.Fatal(err)
	}
	rs, _ := j.Read("2026-10-02")
	if len(rs) != 1 || rs[0].Trigger != TriggerButton {
		t.Fatalf("起動経路が記録されていない: %+v", rs)
	}
}
