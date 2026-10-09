package main

import (
	"path/filepath"
	"strings"
	"testing"

	"stockbot/backend/internal/app/journal"
)

// 🚨 **トラックを足したのに出力先を足し忘れると、別トラックの記録が消える**。
//
// `journal.WriteJSON` は素の `os.WriteFile` で、ファイル名は日付だけ。`-db harvest` を
// 足したとき `defaultOutDir` を広げ忘れて harvest が research の枝に落ちていたので、
// `daily-review -db harvest -date 2026-08-26` が **research の 2026-08-26 パケットを
// 上書き**する形になっていた。しかも `.md` は O_EXCL なので、その日の段2 は
// もう使い切られていて書き直せない。
//
// さらに悪いことに、直前に足した `RefuseEmptyRebuild` が**harvest の測り直しを
// research のファイルと突き合わせる**ので、両方向に誤爆する:
// harvest に行があれば黙って上書きし、harvest が正当に空なら
// 「DB を取り違えている」と嘘の理由で止まる。
func TestEveryTrackGetsItsOwnJournalDirectory(t *testing.T) {
	seen := map[string]string{}
	for _, db := range []string{"research", "harvest", "live"} {
		dir := defaultOutDir(db)
		if prev, dup := seen[dir]; dup {
			t.Fatalf("%s と %s が同じ出力先 %q — ファイル名は日付なので片方が上書きで消える",
				db, prev, dir)
		}
		seen[dir] = db
	}
	// research は既定ディレクトリのまま(既存の launchd / 手順が壊れない)。
	if got := defaultOutDir("research"); got != journal.DefaultDir() {
		t.Errorf("research = %q, want %q(既定のまま)", got, journal.DefaultDir())
	}
	// 空文字("-db" 未指定)は research と同じ扱い。
	if defaultOutDir("") != defaultOutDir("research") {
		t.Error("-db 未指定が research と別扱いになっている")
	}
	for _, db := range []string{"harvest", "live"} {
		if !strings.HasSuffix(defaultOutDir(db), filepath.Join(journal.DefaultDir(), db)) {
			t.Errorf("%s の出力先が %s/ サブディレクトリでない: %q", db, db, defaultOutDir(db))
		}
	}
}
