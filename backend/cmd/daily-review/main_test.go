package main

import (
	"path/filepath"
	"testing"
)

// 🛑 -db live の既定出力先は research と**別ディレクトリ**。ファイル名は日付なので、
// 同居させると片方が上書きで消える(bot 側の引け後ジョブと同じ規約)。
func TestDefaultOutDir_SeparatesLiveFromResearch(t *testing.T) {
	research := defaultOutDir("research")
	live := defaultOutDir("live")
	if research == live {
		t.Fatalf("research と live の出力先が同じ(%q)— 日付ファイル名が衝突する", live)
	}
	if filepath.Base(live) != "live" {
		t.Fatalf("live の出力先 = %q, want .../live", live)
	}
	if defaultOutDir("") != research {
		t.Fatal("既定(空)は research 側であること")
	}
}
