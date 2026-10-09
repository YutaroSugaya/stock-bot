package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// closeAllAlarmArg は指定ファイル内の `command.NewCloseAllOpen(...)` 呼び出しを 1 つ探し、
// **最後の引数(unprotected)のソース表現**を返す。
//
// 🛑 AST で見るのは、この配線が「実行されるまで分からない nil」だったから。
// 以前は closeExecutor を手組みするテストしか無く、**本番のコンストラクタが
// nil をリテラルで固定している**事実を 1 つも見ていなかったので、
// 「live では一度も発火しない trip」を緑のまま通した。
// 振る舞いのテスト(close_all_unprotected_test.go)は「コンストラクタに渡せば鳴る」
// ことを証明するが、**live が実際に渡しているか**はここでしか見られない。
func closeAllAlarmArg(t *testing.T, file string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var got string
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewCloseAllOpen" {
			return true
		}
		if len(call.Args) == 0 {
			t.Fatalf("%s: NewCloseAllOpen に引数が無い", file)
		}
		found = true
		start := fset.Position(call.Args[len(call.Args)-1].Pos()).Offset
		end := fset.Position(call.Args[len(call.Args)-1].End()).Offset
		src := mustReadSource(t, file)
		got = src[start:end]
		return false
	})
	if !found {
		t.Fatalf("%s: NewCloseAllOpen の呼び出しが見つからない — 配線が別の場所へ移ったなら本テストも移すこと", file)
	}
	return got
}

// 🚨 live トラックは **裸の建玉を報せる警報を必ず渡す**。
// 画面の「成行決済」は POST /api/live/positions/close → LiveViews.Close.CloseOne →
// CloseAllOpen へ落ちる。ここが nil だと、守りを cancel した後に決済が板へ載らなくても
// bot は誰にも知らせない(実際には人間が証券アプリで TP を置き直すまで裸だった)。
func TestLiveTrackWiresTheUnprotectedAlarm(t *testing.T) {
	if got := closeAllAlarmArg(t, "live_track.go"); got != "lt.emergency" {
		t.Fatalf("live の NewCloseAllOpen に渡す警報が %q — `lt.emergency` でなければ、"+
			"実弾の建玉が裸になっても誰も気付けない", got)
	}
}

// 🚨 main.go のトラックは **「必ず紙」ではない**。hybrid env
// (STOCKBOT_LIVE_BOT_CONFIG)が無い単一トラック構成では config.ValidateHybrid が
// early return するので research 側 mode の検査が走らず、mode: live_config +
// tachibana でこのトラック自身が実弾になる。
//
// よってここに nil を焼き込むと、live_track.go で塞いだ穴を 1 箇所に植え直すことになる。
// mode を見て分岐していること(= `emergency` と `config.ModeLive` の両方が
// 最終引数の式に現れること)を要求する。
func TestMainTrackAlarmDependsOnMode(t *testing.T) {
	got := closeAllAlarmArg(t, "main.go")
	for _, want := range []string{"config.ModeLive", "emergency"} {
		if !containsToken(got, want) {
			t.Fatalf("main.go の NewCloseAllOpen の警報が %q — mode を見て分岐していない。"+
				"hybrid env 無しの単一トラック構成ではこのトラックが実弾になりうるので、"+
				"%q が式に現れねばならない", got, want)
		}
	}
}

func containsToken(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
