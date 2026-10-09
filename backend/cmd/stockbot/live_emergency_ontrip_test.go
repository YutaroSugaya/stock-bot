package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/app"
	"stockbot/backend/internal/domain/clock"
)

// 🚨 live の trip が**成功**しても、従来はログ行もカウンタも出なかった
// (onTrip に nil を渡していた)。表面はダッシュボードのブール値とフラグファイルだけ。
// 「人間が 1 時間以内に気付ける」ことが価値の全部である一連の修正にとって、
// ここが最弱点 —— 事故そのものが「信号を誰も見ていなかった」形だった。
func TestLiveEmergencyTripLogsAndCounts(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	counters := &app.Counters{}
	es := newLiveEmergency(filepath.Join(t.TempDir(), "f.flag"), counters, logger)

	if err := es.Trip("close_unfilled_unprotected:shinyo:4704", time.Now()); err != nil {
		t.Fatalf("trip: %v", err)
	}

	if got := counters.EmergencyTrips.Load(); got != 1 {
		t.Fatalf("EmergencyTrips=%d, want 1 — /api/live/status に出ない", got)
	}
	out := buf.String()
	if !strings.Contains(out, "LIVE EMERGENCY TRIP") {
		t.Fatalf("trip がログに出ていない: %q — ログファイルが唯一の事後追跡手段", out)
	}
	if !strings.Contains(out, "close_unfilled_unprotected:shinyo:4704") {
		t.Fatalf("理由がログに出ていない: %q — 何が起きたか復元できない", out)
	}
	if !strings.Contains(out, `"track":"live"`) {
		t.Fatalf("track が付いていない: %q — research のログと混ざる", out)
	}
}

// 🛑 counters が nil でも落ちない。onTrip は EmergencyStop の mutex を握ったまま
// 呼ばれるので、ここで panic すると**警報そのものが落ちる**(守るための仕組みが
// 守る瞬間に壊れる、が最悪の形)。
func TestLiveEmergencyToleratesNilCounters(t *testing.T) {
	es := newLiveEmergency(filepath.Join(t.TempDir(), "f.flag"), nil, nil)
	if err := es.Trip("test", time.Now()); err != nil {
		t.Fatalf("trip: %v", err)
	}
	if !es.Active() {
		t.Fatal("trip が効いていない")
	}
}

// 🛑 live トラックの配線が newLiveEmergency を通ることを固定する。
// 直接 safety.NewEmergencyStop(..., nil) に戻されると、また無言に戻る。
func TestLiveTrackBuildsEmergencyThroughTheHelper(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "live_track.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse live_track.go: %v", err)
	}
	var direct, viaHelper int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fn.Sel.Name == "NewEmergencyStop" {
				direct++
			}
		case *ast.Ident:
			if fn.Name == "newLiveEmergency" {
				viaHelper++
			}
		}
		return true
	})
	if viaHelper == 0 {
		t.Fatal("live_track.go が newLiveEmergency を使っていない — trip が無言に戻る")
	}
	if direct != 1 {
		t.Fatalf("live_track.go が safety.NewEmergencyStop を %d 回直接呼んでいる — "+
			"newLiveEmergency 内の 1 回だけであるべき(別経路で nil onTrip が復活する)", direct)
	}
}

var _ = clock.JST
