package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// DB プールの所有権ガード。
//
// 🚨 なぜ要るか: store / live はそれぞれ自前の pgx プールを持ち、
// その解放は `closeFn` 1 本に集約されている。**closeFn を defer してよいのは
// プロセスの寿命を持つ `run` だけ**で、組み立てのヘルパ関数の中で defer すると
// **そのヘルパが return した瞬間**にプールが閉じる。以降 DB 操作は全て
// `closed pool` で失敗するが、reconcile の失敗は WARN なので**起動は成功したまま
// 静かに死ぬ**(fatal も panic も出ない)。
//
// 実際に起きた: 起動を runtimeState の 10 フェーズに分ける refactor で
// `st` と `live` の defer は `run` へ引き上げたのに **harvest の 1 つだけ
// `buildTracks` に残った**。起動で harvest トラックの全建玉が
// 毎 30 秒 `reconcile failed / closed pool` を吐き、台帳の読み書きが全滅していた。
// live と research は無事だったので、金銭的被害ではなく**測定の欠落**として現れる。
//
// grep ではなく AST で見るのは、コメントや文字列の中の言及を拾わないため。
func TestCloseFnIsDeferredOnlyByRun(t *testing.T) {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list cmd/stockbot: %v", err)
	}
	files := map[string]*ast.File{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if f.Name.Name == "main" {
			files[path] = f
		}
	}
	if len(files) == 0 {
		t.Fatal("cmd/stockbot の package main のファイルが 1 本も見つからない(検査が空回りする)")
	}

	var offenders []string
	for path, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			// run はプロセスの寿命を持つので、ここでの defer が正しい所有者。
			if fn.Name.Name == "run" && fn.Recv == nil {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				def, ok := n.(*ast.DeferStmt)
				if !ok {
					return true
				}
				sel, ok := def.Call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "closeFn" {
					return true
				}
				pos := fset.Position(def.Pos())
				offenders = append(offenders, path+":"+itoa(pos.Line)+" ("+funcLabel(fn)+")")
				return true
			})
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("closeFn を defer してよいのは run だけ(ヘルパ内の defer は return 時に"+
			"プールを閉じ、以降の DB 操作が全て closed pool で静かに失敗する):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func funcLabel(fn *ast.FuncDecl) string {
	if fn.Recv == nil {
		return fn.Name.Name
	}
	return "(recv)." + fn.Name.Name
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
