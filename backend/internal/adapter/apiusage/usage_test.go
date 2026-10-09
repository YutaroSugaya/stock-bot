package apiusage

import (
	"os"
	"path/filepath"
	"stockbot/backend/internal/domain/clock"
	"sync"
	"testing"
	"time"
)

func testJST(t *testing.T) *time.Location {
	t.Helper()
	loc := clock.JST
	return loc
}

func at(t *testing.T, y int, m time.Month, d, h, min int) time.Time {
	t.Helper()
	return time.Date(y, m, d, h, min, 0, 0, testJST(t))
}

// 立花の集計期間は開局している時間帯(5:30〜翌3:30)。暦日ではないので、
// 深夜0時を跨いでも同じ窓に積み上がる — ここを暦日で切ると、夜間の日足取得が
// 翌日ぶんに計上されて bot 側の数と立花側の計測が永久にずれる。
func TestWindowStartFollowsTachibanaTradingDay(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"開局直後", at(t, 2026, 8, 12, 5, 30), at(t, 2026, 8, 12, 5, 30)},
		{"朝の日足取得", at(t, 2026, 8, 12, 7, 0), at(t, 2026, 8, 12, 5, 30)},
		{"場中", at(t, 2026, 8, 12, 14, 0), at(t, 2026, 8, 12, 5, 30)},
		{"日付を跨いだ夜間", at(t, 2026, 8, 13, 1, 0), at(t, 2026, 8, 12, 5, 30)},
		{"閉局直前", at(t, 2026, 8, 13, 3, 29), at(t, 2026, 8, 12, 5, 30)},
		{"閉局中(3:30〜5:30)は直前の窓に residual として積む", at(t, 2026, 8, 13, 4, 0), at(t, 2026, 8, 12, 5, 30)},
		{"開局5分前もまだ前の窓", at(t, 2026, 8, 13, 5, 25), at(t, 2026, 8, 12, 5, 30)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WindowStart(c.now); !got.Equal(c.want) {
				t.Errorf("WindowStart(%s) = %s, want %s", c.now, got, c.want)
			}
		})
	}
}

// 🛑 これが B の要件そのもの: **再起動しても当日のカウントは 0 に戻らない**。
// プロセス内 atomic だけだった旧 api_requests は make stop/start のたびにゼロに
// 戻り、その日の実数を誰も言えなかった(make stop/start は任意タイミングで打たれる)。
//
// 🛑 **name は同じ("stockbot")で開き直すこと。** 最初の版はここを "proc-a" /
// "proc-b" と**別名**にしていたため緑になっていたが、本番の再起動は同じ
// `make start` = 同じ name であり、**ファイル名が衝突して前のプロセスのぶんを
// 上書きしていた**(実測: 3,320 → 4 にリセット)。
// 「別プロセス」を別名で模したテストは、再起動を一度も検査していなかった。
func TestCountSurvivesProcessRestart(t *testing.T) {
	dir := t.TempDir()
	now := at(t, 2026, 8, 12, 9, 0)
	clk := func() time.Time { return now }

	first := Open(dir, "stockbot", clk)
	for i := 0; i < 5; i++ {
		first.Record("CLMMfdsGetMarketPrice")
	}

	// make stop → make start。**同じ name** で開き直す。
	now = at(t, 2026, 8, 12, 10, 0)
	second := Open(dir, "stockbot", clk)
	second.Record("CLMMfdsGetMarketPrice")

	got := second.Snapshot()
	if got.Total != 6 {
		t.Errorf("再起動後の合計=%d, want 6(前のプロセスぶんが消えている)", got.Total)
	}
	if got.ByCLMID["CLMMfdsGetMarketPrice"] != 6 {
		t.Errorf("CLMID 別=%v, want 6", got.ByCLMID)
	}
	if !got.WindowStart.Equal(at(t, 2026, 8, 12, 5, 30)) {
		t.Errorf("窓の起点=%s", got.WindowStart)
	}
}

// 立花の内訳(CLMMfdsGetMarketPrice / History / Logout)と直接突き合わせられる形で
// 残す。総数だけだと「どの処理が原因か」の問いに二度と答えられない。
func TestSnapshotBreaksDownByCLMID(t *testing.T) {
	dir := t.TempDir()
	now := at(t, 2026, 8, 12, 9, 0)
	c := Open(dir, "bot", func() time.Time { return now })

	c.Record("CLMMfdsGetMarketPrice")
	c.Record("CLMMfdsGetMarketPrice")
	c.Record("CLMMfdsGetMarketPriceHistory")
	c.Record("CLMAuthLoginRequest")

	got := c.Snapshot()
	if got.Total != 4 {
		t.Fatalf("total=%d, want 4", got.Total)
	}
	if got.ByCLMID["CLMMfdsGetMarketPrice"] != 2 || got.ByCLMID["CLMMfdsGetMarketPriceHistory"] != 1 {
		t.Fatalf("内訳=%v", got.ByCLMID)
	}
	// 立花側の集計は CLMAuthLoginRequest を数えない(auth ホスト宛のみ別扱い)。
	// bot は数えるが、立花側基準の比較用に login を除いた数も出せること。
	if got.TotalExcludingLogin != 3 {
		t.Errorf("login を除いた数=%d, want 3", got.TotalExcludingLogin)
	}
}

// 窓が変われば数え直す(前日ぶんが今日に足し込まれない)。
func TestCountResetsOnNewWindow(t *testing.T) {
	dir := t.TempDir()
	now := at(t, 2026, 8, 12, 14, 0)
	clk := func() time.Time { return now }
	c := Open(dir, "bot", clk)
	c.Record("CLMMfdsGetMarketPrice")
	c.Record("CLMMfdsGetMarketPrice")
	if got := c.Snapshot().Total; got != 2 {
		t.Fatalf("当日=%d, want 2", got)
	}

	now = at(t, 2026, 8, 13, 6, 0) // 翌開局
	c.Record("CLMMfdsGetMarketPrice")
	got := c.Snapshot()
	if got.Total != 1 {
		t.Errorf("翌窓の合計=%d, want 1(前の窓が混ざっている)", got.Total)
	}
	if !got.WindowStart.Equal(at(t, 2026, 8, 13, 5, 30)) {
		t.Errorf("窓の起点=%s, want 2026-08-13 05:30", got.WindowStart)
	}
}

// bot と fetch-daily は同時に走る。互いのぶんを踏み潰さないこと(1日の総数は
// **プロセス単位ではなく口座単位**で数えられているため、合算できないと意味が無い)。
func TestConcurrentProcessesAreSummedNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	now := at(t, 2026, 8, 12, 7, 0)
	clk := func() time.Time { return now }

	bot := Open(dir, "stockbot", clk)
	fetch := Open(dir, "fetch-daily", clk)

	var wg sync.WaitGroup
	for _, c := range []*Counter{bot, fetch} {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				c.Record("CLMMfdsGetMarketPriceHistory")
			}
		}()
	}
	wg.Wait()

	if got := bot.Snapshot().Total; got != 200 {
		t.Errorf("2プロセス合算=%d, want 200", got)
	}
}

// 🛑 観測は取引を止めない。書けない場所を渡されても Record は素通りすること
// (カウンタのために発注経路が落ちるのは本末転倒)。
func TestRecordIsFailSoftWhenStateDirIsUnwritable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Open(dir, "bot", func() time.Time { return at(t, 2026, 8, 12, 9, 0) })
	c.Record("CLMMfdsGetMarketPrice") // panic しないこと
	// 永続化できなくても、そのプロセス内の数字までは失わない。
	if got := c.Snapshot().Total; got != 1 {
		t.Errorf("プロセス内の合計=%d, want 1", got)
	}
}

// 古い窓のファイルが無限に溜まらない。
func TestOpenPrunesOldWindows(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "20260701-old.json")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte(`{"window_start":"2026-07-01T05:30:00+09:00","total":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	Open(dir, "bot", func() time.Time { return at(t, 2026, 8, 12, 9, 0) })
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("古い窓のファイルが残っている: %v", err)
	}
}

// 🛑 bot と fetch-daily が同じディレクトリを見ることが合算の前提。置き場の決定が
// 2 か所にあると、片方だけ env を渡した日に静かに合算されなくなる。
func TestDefaultDirIsUnderStockbotHomeAndEnvOverridable(t *testing.T) {
	t.Setenv("STOCKBOT_API_USAGE_DIR", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("HOME 不明の環境")
	}
	want := filepath.Join(home, ".stockbot", "state", "api-usage")
	if got := DefaultDir(); got != want {
		t.Errorf("DefaultDir()=%q, want %q(Desktop 配下は TCC で launchd から触れない)", got, want)
	}
	t.Setenv("STOCKBOT_API_USAGE_DIR", "/tmp/xx-usage")
	if got := DefaultDir(); got != "/tmp/xx-usage" {
		t.Errorf("env 上書きが効いていない: %q", got)
	}
}
