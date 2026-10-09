package main

import (
	"os"
	"testing"
)

// mustReadSource はソースを文字列で読む(配線の guard test 用)。harvest_track_test.go の
// 削除に伴い共通ヘルパーとしてここへ移した。
func mustReadSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
