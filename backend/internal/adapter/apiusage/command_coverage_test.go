package apiusage_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryTachibanaCommandRecordsUsage は、立花のアダプタを作るコマンドが必ず永続カウンタを
// 挿すことを固定する。
//
// 🚨 立花は閉局・休日・メンテ時間帯のアクセスを禁じている。これを調べたとき、
// live-probe と loanable-fetch は SetUsageRecorder を挿しておらず、api-usage に 1 行も
// 残らなかった。いつ叩いたかを答えられない経路は、指摘の原因から外すことができない。
func TestEveryTachibanaCommandRecordsUsage(t *testing.T) {
	cmdRoot := filepath.Join("..", "..", "..", "cmd")
	dirs, err := os.ReadDir(cmdRoot)
	if err != nil {
		t.Fatalf("cmd を読めない: %v", err)
	}
	found := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		src := codeOf(t, filepath.Join(cmdRoot, d.Name()))
		if !strings.Contains(src, "broker.NewTachibana(") {
			continue
		}
		found++
		if !strings.Contains(src, ".SetUsageRecorder(") {
			t.Errorf("cmd/%s は立花のアダプタを作るのに SetUsageRecorder を挿していない — api-usage に残らない通信になる", d.Name())
		}
	}
	// 走査そのものが壊れて 0 本で緑になるのを防ぐ(現在 4 本)。
	if found < 4 {
		t.Fatalf("立花のアダプタを作るコマンドが %d 本しか見つからない — 走査が壊れている", found)
	}
}

// codeOf はパッケージの本体(テスト以外)のソースを、行コメントを除いて連結する。
func codeOf(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.SplitSeq(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
