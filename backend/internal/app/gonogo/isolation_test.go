package gonogo

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 🛑 **LLM の go/no-go は表示と記録だけ**(CLAUDE.md §4)。発注経路のパッケージは判定を
// import しない・参照しない。人間が止めるのはボタン(manual_symbol_block)で、LLM ではない。
func TestOrderPathDoesNotReadGoNoGo(t *testing.T) {
	orderPath := []string{
		"../../usecase/command",
		"../../domain/risk",
		"../../domain/strategy",
		"../../domain/order",
		"../../domain/position",
	}
	banned := []string{"stockbot/backend/internal/adapter/gonogo", "stockbot/backend/internal/app/gonogo"}
	for _, dir := range orderPath {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s を読めない(テストが対象を見失っている): %v", dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			ast, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			for _, imp := range ast.Imports {
				for _, b := range banned {
					if strings.Trim(imp.Path.Value, `"`) == b {
						t.Errorf("%s が %s を import している — 発注経路が LLM の判定を読む", f, b)
					}
				}
			}
			src, _ := os.ReadFile(f)
			if strings.Contains(string(src), "GoNoGo") {
				t.Errorf("%s が GoNoGo を参照している — 発注経路が LLM の判定を読む", f)
			}
		}
	}
}
