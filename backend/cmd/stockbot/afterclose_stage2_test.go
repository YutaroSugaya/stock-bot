package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/app"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/session"
)

func writeJSON(t *testing.T, dir, day, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, day+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stubDeps(dir string, now time.Time) stage2Deps {
	return stage2Deps{cli: "claude", outDir: dir, dbURL: "postgres://x", dataDir: dir, now: now}
}

// 🛑 **段1 を測り直した結果**を段2 に渡す(引け直後に組んだ JSON は市況が空)。
// 1日が失敗しても残りの日は書く — 1日の失敗で他の日を落とさない。
func TestRunStage2Backfill_RebuildsThenWritesAndSurvivesOneFailure(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "2026-08-12", `{"stale":1}`)
	writeJSON(t, dir, "2026-08-13", `{"stale":2}`)

	d := stubDeps(dir, time.Date(2026, 8, 14, 16, 0, 0, 0, clock.JST))
	rebuilt := map[string]bool{}
	d.rebuild = func(_ context.Context, _, _, _, date string) ([]byte, error) {
		rebuilt[date] = true
		return []byte(`{"fresh":"` + date + `"}`), nil
	}
	got := map[string]string{}
	d.run = func(_ context.Context, _, _, date string, packet []byte, _ time.Duration) (string, error) {
		got[date] = string(packet)
		if date == "2026-08-13" {
			return "", errors.New("CLI が落ちた")
		}
		return date + ".md", nil
	}

	err := runStage2Backfill(context.Background(), d)
	if err == nil {
		t.Fatal("失敗した日があるのに error を返していない(ログに出ない)")
	}
	if !rebuilt["2026-08-12"] || !rebuilt["2026-08-13"] {
		t.Fatalf("段1 を測り直していない: %v", rebuilt)
	}
	if got["2026-08-12"] != `{"fresh":"2026-08-12"}` || got["2026-08-13"] != `{"fresh":"2026-08-13"}` {
		t.Fatalf("古い JSON を渡している(市況が空のまま固定される): %v", got)
	}
	// 失敗した日も「今日は試した」印が残る = 再起動しても投げ直さない。
	for _, day := range []string{"2026-08-12", "2026-08-13"} {
		if _, err := os.Stat(filepath.Join(dir, day+".stage2.attempt")); err != nil {
			t.Fatalf("%s の試行印が無い: %v", day, err)
		}
	}
}

// 🛑 **停止で中断された試行は「試した」に数えない。** 15:40 の直後に停止されると
// 段2 は毎回途中で切られる。印を残したままだとその日はもう書かれず、**毎日欠測する**。
// 印を消しておけば次の起動でそのまま書ける(1回きりの試行に賭けない)。
func TestRunStage2Backfill_ShutdownCancellationIsNotAnAttempt(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "2026-08-13", `{}`)

	ctx, cancel := context.WithCancel(context.Background())
	d := stubDeps(dir, time.Date(2026, 8, 14, 16, 0, 0, 0, clock.JST))
	d.rebuild = func(context.Context, string, string, string, string) ([]byte, error) { return []byte(`{}`), nil }
	d.run = func(context.Context, string, string, string, []byte, time.Duration) (string, error) {
		cancel() // 停止シグナルで ctx が切れる
		return "", context.Canceled
	}
	if err := runStage2Backfill(ctx, d); err == nil {
		t.Fatal("中断を成功として扱った")
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-08-13.stage2.attempt")); !os.IsNotExist(err) {
		t.Fatal("停止で切られた試行の印が残っている — その日は二度と書かれない")
	}
}

// 書くものが無いときは claude も DB も触らない(引け後に空打ちしない)。
func TestRunStage2Backfill_NoPendingDaysIsANoop(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "2026-08-13", `{}`)
	if err := os.WriteFile(filepath.Join(dir, "2026-08-13.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := stubDeps(dir, time.Date(2026, 8, 14, 16, 0, 0, 0, clock.JST))
	calls := 0
	d.rebuild = func(context.Context, string, string, string, string) ([]byte, error) { calls++; return nil, nil }
	d.run = func(context.Context, string, string, string, []byte, time.Duration) (string, error) {
		calls++
		return "", nil
	}
	if err := runStage2Backfill(context.Background(), d); err != nil {
		t.Fatalf("noop で error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("calls = %d, want 0", calls)
	}
}

// 🛑 段2 の枠は「1回でも完走できる長さ」であること。同じ `--effort max` を使う
// advisor の実測は 621秒 で、そのために無制限に倒してある。短く切ると毎日 timeout
// する無音ループになる(レビュー指摘)。
func TestStage2Budget(t *testing.T) {
	const advisorMeasured = 621 * time.Second
	if stage2PerDayTimeout <= advisorMeasured {
		t.Fatalf("1日あたり %v は advisor の実測 %v 以下 — 毎日 timeout する", stage2PerDayTimeout, advisorMeasured)
	}
	if stage2MaxDays*stage2PerDayTimeout > stage2JobTimeout {
		t.Fatalf("%d日 × %v > ジョブ枠 %v", stage2MaxDays, stage2PerDayTimeout, stage2JobTimeout)
	}
}

func acCfg(mode config.Mode) *config.BotConfig {
	return &config.BotConfig{Mode: mode}
}

func acTradingHours() session.TradingHours {
	return session.TradingHours{TZ: clock.JST, ForceFlatAt: "14:50",
		Sessions:        []session.Window{{Start: "09:00", End: "15:30"}},
		CalendarThrough: time.Date(2026, 12, 31, 0, 0, 0, 0, clock.JST)}
}

// 配線そのものを固定する。ここが無いと `AddSlow` を `Add` に戻しても、live で
// 段2 が登録されるようになっても、全部緑のまま通ってしまう(レビュー指摘)。
func TestNewAfterCloseJobs_Stage2Wiring(t *testing.T) {
	t.Setenv("STOCKBOT_DATABASE_URL", "postgres://x/y")
	t.Setenv("STOCKBOT_DAILY_REVIEW_STAGE2", "on") // 段 2 は opt-in
	clk := func() time.Time { return time.Date(2026, 8, 14, 16, 0, 0, 0, clock.JST) }

	t.Run("paper では登録され、停止経路では走らない", func(t *testing.T) {
		ac := newAfterCloseJobs(acCfg(config.ModePaper), acTradingHours(), clk, nil)
		names := jobNames(ac)
		if !contains(names, "daily-review-stage2") {
			t.Fatalf("段2 が登録されていない: %v", names)
		}
		if slowJobs(ac) != 1 {
			t.Fatal("段2 が AddSlow で登録されていない(make stop 後もプロセスが居残る)")
		}
	})
	t.Run("live では登録しない", func(t *testing.T) {
		names := jobNames(newAfterCloseJobs(acCfg(config.ModeLive), acTradingHours(), clk, nil))
		if contains(names, "daily-review-stage2") {
			t.Fatalf("live で LLM 子プロセスが起きる配線になっている: %v", names)
		}
	})
	t.Run("off で止められる", func(t *testing.T) {
		t.Setenv("STOCKBOT_DAILY_REVIEW_STAGE2", "off")
		names := jobNames(newAfterCloseJobs(acCfg(config.ModePaper), acTradingHours(), clk, nil))
		if contains(names, "daily-review-stage2") {
			t.Fatalf("off が効いていない: %v", names)
		}
	})
	t.Run("DB が無ければ登録しない", func(t *testing.T) {
		t.Setenv("STOCKBOT_DATABASE_URL", "")
		names := jobNames(newAfterCloseJobs(acCfg(config.ModePaper), acTradingHours(), clk, nil))
		if contains(names, "daily-review-stage2") || contains(names, "daily-review") {
			t.Fatalf("台帳が無いのに記録ジョブが登録されている: %v", names)
		}
	})
}

func jobNames(ac *app.AfterClose) []string { return ac.JobNames() }

func slowJobs(ac *app.AfterClose) int { return ac.SlowJobCount() }

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// 朝枠の配線: 段2 と同じ条件(paper・DB あり・off でない)でだけ登録する。
// live では登録しない(LLM 子プロセスを起こさない不変条件は朝も同じ)。
func TestNewAfterCloseJobs_Stage2MorningWiring(t *testing.T) {
	t.Setenv("STOCKBOT_DATABASE_URL", "postgres://x/y")
	t.Setenv("STOCKBOT_DAILY_REVIEW_STAGE2", "on") // 段 2 は opt-in
	clk := func() time.Time { return time.Date(2026, 8, 14, 7, 30, 0, 0, clock.JST) }
	if names := jobNames(newAfterCloseJobs(acCfg(config.ModePaper), acTradingHours(), clk, nil)); !contains(names, "daily-review-stage2-morning") {
		t.Fatalf("paper で朝枠が登録されていない: %v", names)
	}
	if names := jobNames(newAfterCloseJobs(acCfg(config.ModeLive), acTradingHours(), clk, nil)); contains(names, "daily-review-stage2-morning") {
		t.Fatalf("live で朝枠(LLM)が登録されている: %v", names)
	}
	t.Setenv("STOCKBOT_DAILY_REVIEW_STAGE2", "off")
	if names := jobNames(newAfterCloseJobs(acCfg(config.ModePaper), acTradingHours(), clk, nil)); contains(names, "daily-review-stage2-morning") {
		t.Fatalf("off が朝枠に効いていない: %v", names)
	}
}

// 朝枠の ready = 「今朝のユニバース選定が済んだ」(= 朝の日足取得が終わった)。
// 日足取得の途中で段2 を書くと、欠けた市況の総評が O_EXCL で永久に固定される。
func TestUniverseSelectedToday(t *testing.T) {
	path := filepath.Join(t.TempDir(), "today.txt")
	now := time.Date(2026, 9, 24, 7, 45, 0, 0, clock.JST)
	if universeSelectedToday(path, now) {
		t.Fatal("ファイルが無いのに ready")
	}
	if err := os.WriteFile(path, []byte("7203\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yesterday := time.Date(2026, 9, 23, 7, 40, 0, 0, clock.JST)
	if err := os.Chtimes(path, yesterday, yesterday); err != nil {
		t.Fatal(err)
	}
	if universeSelectedToday(path, now) {
		t.Fatal("昨日の選定なのに ready(日足取得がまだ)")
	}
	today := time.Date(2026, 9, 24, 7, 40, 0, 0, clock.JST)
	if err := os.Chtimes(path, today, today); err != nil {
		t.Fatal(err)
	}
	if !universeSelectedToday(path, now) {
		t.Fatal("今朝の選定が済んでいるのに ready でない")
	}
}
