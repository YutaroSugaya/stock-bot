package main

import (
	"path/filepath"
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/testutil"
)

// 🛑 JPX のストップ配分 CSV の取り込みは**やめた**。8 月以降 37 回中 37 回が
// HTTP 404 で一度も取れておらず、取引判断にも使っていない(研究用の母数だった)。戻すなら
// 404 の原因(公開時刻か URL か)を先に直すこと(取り込みの cmd と adapter は削除した。git の履歴にある)。
// 既存の jpx_stops.csv は残してある(fetch-daily が退避する)。
func TestAfterCloseJobs_DoesNotRegisterJPXStops(t *testing.T) {
	t.Setenv("STOCKBOT_DATABASE_URL", "postgres://u:p@localhost:5434/stockbot?sslmode=disable")
	t.Setenv("STOCKBOT_DAILY_REVIEW_STAGE2", "off")
	if names := afterCloseJobNames(t); hasJob(names, "jpx-stops") {
		t.Fatalf("JPX の取り込みがまだ登録されている: %v", names)
	}
}

// 🛑 引け後の日次総評は research DB しか見ていなかったので、**実弾トラックの 1 日が
// 引け後の記録に一行も残らなかった**(実弾が動いた日でも)。
// live 用のジョブが登録されること、そして **live の出力先が research と別**であることを
// 固定する(同じディレクトリに書くと日付ファイル名が衝突して片方が消える)。
func TestAfterCloseJobs_RegistersLiveJournalWhenLiveDBConfigured(t *testing.T) {
	t.Setenv("STOCKBOT_DATABASE_URL", "postgres://u:p@localhost:5434/stockbot?sslmode=disable")
	t.Setenv("STOCKBOT_DAILY_REVIEW_STAGE2", "off")

	t.Setenv("STOCKBOT_LIVE_DATABASE_URL", "")
	if names := afterCloseJobNames(t); hasJob(names, "daily-review-live") {
		t.Fatalf("live DB 未設定なのに live ジョブが登録された: %v", names)
	}

	t.Setenv("STOCKBOT_LIVE_DATABASE_URL", "postgres://u:p@localhost:5434/stockbot_live?sslmode=disable")
	names := afterCloseJobNames(t)
	if !hasJob(names, "daily-review") {
		t.Fatalf("research の日次総評が消えている: %v", names)
	}
	if !hasJob(names, "daily-review-live") {
		t.Fatalf("live トラックの日次総評が登録されていない: %v", names)
	}
}

// live の出力先は research と別ディレクトリ(ファイル名は日付なので混ぜると衝突する)。
func TestJournalDirForTrack_SeparatesLiveFromResearch(t *testing.T) {
	research := journalDirForTrack("")
	live := journalDirForTrack("live")
	if research == live {
		t.Fatalf("research と live の journal 出力先が同じ(%q)— 日付ファイル名が衝突する", live)
	}
	if filepath.Base(live) != "live" {
		t.Fatalf("live の出力先が %q — トラック名で分けること", live)
	}
}

func afterCloseJobNames(t *testing.T) []string {
	t.Helper()
	t.Setenv("STOCKBOT_DAILY_CANDLES_DIR", t.TempDir())
	hours := session.TradingHours{
		TZ:       clock.JST,
		Sessions: []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:00"}},
	}
	ac := newAfterCloseJobs(&config.BotConfig{Mode: config.ModePaper}, hours, clock.System(), testutil.SilentLogger())
	return ac.JobNames()
}

func hasJob(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
