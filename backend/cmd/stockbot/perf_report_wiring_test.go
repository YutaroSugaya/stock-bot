package main

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/query"
)

// MOCK rationale (TESTING.md 3用途): §1 system boundary(query の下の repository)。
// 戦績の組み立ては ListClosedSince しか呼ばない。残りは埋め込みの nil のままで、
// 呼ばれたら panic する = 読み取り経路が書き込み系に触っていないことを型で保つ。
type stubClosedTrades struct{ port.TradeRepository }

func (stubClosedTrades) ListClosedSince(context.Context, time.Time) ([]port.TradeRecord, error) {
	return nil, nil
}

// 🛑 画面の戦績は**口座ベース**。entry_compensated /
// external_close を別枠に逃がさず普通のトレードとして計上する。
// エッジ標本(cmd/forward-report → cmd/edge-judge)は既定のまま除外を維持する。
func TestPerfReportOptionsCountNonStrategyCloses(t *testing.T) {
	rep, err := query.NewBuildForwardReport(stubClosedTrades{}, perfReportOptions(nil)...).
		Execute(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rep.Counting != query.CountingAccount {
		t.Fatalf("Counting = %q, want %q — 画面が戦略外の決済を落としている", rep.Counting, query.CountingAccount)
	}
}

// 🛑 research(main.go)と live(live_track.go)で**別々に組み立てている**ので、
// 片方だけ直す差分が緑で通る。両方が同じ組み立て関数を通ることを固定する
// (「転送漏れで live だけ旧経路に落ちた」のと同じ形の事故)。
func TestBothTracksBuildPerformanceThroughPerfReportOptions(t *testing.T) {
	call := regexp.MustCompile(`query\.NewBuildForwardReport\([^)]*\)`)
	for _, f := range []string{"main.go", "live_track.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		hits := call.FindAllString(string(b), -1)
		if len(hits) == 0 {
			t.Fatalf("%s に query.NewBuildForwardReport の呼び出しが無い", f)
		}
		for _, h := range hits {
			if !strings.Contains(h, "perfReportOptions(") {
				t.Errorf("%s: %s が perfReportOptions を通っていない — 片方のトラックだけ数え方がずれる", f, h)
			}
		}
	}
}
