package main

import (
	"strings"
	"testing"
)

// 🚨 起動ログは長らく `using postgres persistence` としか言わなかった。**どの台帳に
// 書いているかが起動時に判らない**。
//
// DB を切り替えるときは、人間が `.env` の `STOCKBOT_DATABASE_URL` を書き換える。
// 忘れたまま `make start` すると、**新しい設定の arm 挙動が前の台帳に書き込まれる**。
// DSN の同一判定は live トラックとの間にしか無いので、research 単体の取り違えには
// 誰も気づけない。
//
// 構成で塞げない以上、せめて**画面に出す**。ログに DB 名が出ていれば、切り替え忘れは
// 起動直後の 1 行で判る。
//
// 🛑 認証情報は絶対に出さない — ログは journal に残り、DSN にはパスワードが入っている。
func TestDSNDatabaseNameShowsTheLedgerWithoutLeakingCredentials(t *testing.T) {
	cases := []struct{ dsn, want string }{
		{"postgres://stockbot:hunter2@localhost:5434/stockbot_c3?sslmode=disable", "stockbot_c3"},
		{"postgres://stockbot:hunter2@localhost:5434/stockbot", "stockbot"},
		{"postgres://localhost:5434/stockbot_live?sslmode=disable", "stockbot_live"},
		// 壊れた/想定外の DSN でも**パスワードを露出させない**(空を返す)。
		{"not a dsn", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := dsnDatabaseName(c.dsn)
		if got != c.want {
			t.Errorf("dsnDatabaseName(%q) = %q, want %q", c.dsn, got, c.want)
		}
		if strings.ContainsAny(got, "@:/") || strings.Contains(got, "hunter2") {
			t.Fatalf("認証情報かホストが混ざった: %q", got)
		}
	}
}
