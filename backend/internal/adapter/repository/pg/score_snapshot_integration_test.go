//go:build integration

package pg

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

// score の一次ソースは **screen_snapshots**。advisor(LLM)を arm 経路から
// 外すと advisor_runs は書かれなくなるので、旧経路だけでは決定論 arm のトレードが丸ごと
// 復元不能になる。
//
// 🛑 このテストは**最初のコミットに入っていなかった**(heredoc を含むコマンドが hook に
// 弾かれ、書かれていないのに `make test-integration` の緑を「通った」と読んだ)。
// unit テストは build tag のせいでこの SQL を一度も実行しない。**緑は動く証拠ではない。**
func TestPg_ScoreFromScreenSnapshots(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM screen_snapshots`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM advisor_runs WHERE run_id LIKE 'run-snap-%'`); err != nil {
		t.Fatal(err)
	}

	openedAt := time.Date(2026, 8, 14, 10, 0, 0, 0, clock.JST)
	cfgRepo := NewConfigRepo(pool)
	mkCfg := func(id, sym, strat, runID string) {
		t.Helper()
		if err := cfgRepo.EnsureExists(ctx, port.StrategyConfigRecord{ConfigID: id, Symbol: sym,
			Mode: "paper_config", StrategyName: strat, Status: "expired", RawYAML: "y", AdvisorRunID: runID}); err != nil {
			t.Fatalf("cfg %s: %v", id, err)
		}
	}
	mkPos := func(sym, cfgID string, at time.Time) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO positions (symbol, side, quantity, entry_price, config_id, holding_mode, exec_kind, status, opened_at)
			VALUES ($1,'BUY',100,1000,$2,'multiday','cash','CLOSED',$3) RETURNING id`, sym, cfgID, at).Scan(&id); err != nil {
			t.Fatalf("pos %s: %v", sym, err)
		}
		return id
	}

	mkCfg("20260814-090000-7203", "7203", "atr_breakout_v2", "")
	mkCfg("20260814-090000-6501", "6501", "donchian_breakout_v2", "run-snap-A")
	posSnap := mkPos("7203", "20260814-090000-7203", openedAt)
	posFallback := mkPos("6501", "20260814-090000-6501", openedAt)

	// 同じ銘柄に**戦略違い**の行が並ぶ(実運用そのもの)。取り違えたら別戦略の score を貼る。
	if err := NewScreenSnapshotRepo(pool).InsertRound(ctx, []port.ScreenSnapshot{
		{RoundAt: openedAt.AddDate(0, 0, -1), Symbol: "7203", Strategy: "atr_breakout_v2", Triggered: true, Score: 9.99},
		{RoundAt: openedAt.Add(-90 * time.Minute), Symbol: "7203", Strategy: "atr_breakout_v2", Triggered: true, Score: 1.10},
		{RoundAt: openedAt.Add(-10 * time.Minute), Symbol: "7203", Strategy: "atr_breakout_v2", Triggered: true, Score: 1.42},
		{RoundAt: openedAt.Add(-10 * time.Minute), Symbol: "7203", Strategy: "abs_momentum_v2", Triggered: true, Score: 7.77},
		{RoundAt: openedAt.Add(time.Hour), Symbol: "7203", Strategy: "atr_breakout_v2", Triggered: true, Score: 5.00},
	}); err != nil {
		t.Fatal(err)
	}
	// fallback 用: 6501 は snapshot を持たず advisor_runs だけ持つ。
	if err := NewAdvisorRunRepo(pool).Insert(ctx, port.AdvisorRunRecord{
		RunID: "run-snap-A", Symbol: "6501", Status: port.AdvisorRunSuccess, StartedAt: openedAt,
		InputJSON: `{"advisory":{"screens":[{"strategy":"donchian_breakout_v2","score":1.23}]}}`,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := NewScoreRepo(pool).ScoreByPositionID(ctx, []int64{posSnap, posFallback})
	if err != nil {
		t.Fatalf("ScoreByPositionID: %v", err)
	}
	if got[posSnap].Score != 1.42 {
		t.Fatalf("snapshot score = %v, want 1.42(建玉の直前ラウンド。前日 9.99・未来 5.00・別戦略 7.77 ではない)", got[posSnap].Score)
	}
	if got[posSnap].RunID != "" {
		t.Fatalf("RunID = %q — snapshot 由来の score に run を紐付けない", got[posSnap].RunID)
	}
	if got[posFallback].Score != 1.23 || got[posFallback].RunID != "run-snap-A" {
		t.Fatalf("fallback = %+v, want 1.23 / run-snap-A", got[posFallback])
	}

	// 🛑 同じ JST 日にラウンドが無ければ**復元しない**(前日の score を貼らない)。
	// 24 時間窓のままだとここが通ってしまう。
	if _, err := pool.Exec(ctx, `DELETE FROM screen_snapshots WHERE round_at > $1`, openedAt.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}
	got, err = NewScoreRepo(pool).ScoreByPositionID(ctx, []int64{posSnap})
	if err != nil {
		t.Fatalf("ScoreByPositionID: %v", err)
	}
	if _, ok := got[posSnap]; ok {
		t.Fatalf("前日の score を貼ってはいけない: %+v", got[posSnap])
	}
}
