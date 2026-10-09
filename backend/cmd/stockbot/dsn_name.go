package main

import (
	"net/url"
	"sort"
)

// dsnDatabaseName は DSN から **DB 名だけ**を取り出す。
//
// 🛑 ユーザ名・パスワード・ホストは返さない。用途は起動ログで「今どの台帳に
// 書いているか」を人間に見せることだけで、DSN 全体を出すとパスワードが journal に
// 残る。解釈できない DSN では空を返す(推測して中途半端に露出させない)。
func dsnDatabaseName(dsn string) string {
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		return ""
	}
	if len(u.Path) < 2 {
		return ""
	}
	return u.Path[1:]
}

// usedDatabaseNames は渡された DSN 群の database 名(重複排除・昇順)。
// 空の DSN と解釈できない DSN は落とす — 名前が分からないものを監視対象に
// 加えると、存在しない DB で鳴き続ける。
func usedDatabaseNames(dsns ...string) []string {
	seen := map[string]bool{}
	for _, d := range dsns {
		if d == "" {
			continue
		}
		if n := dsnDatabaseName(d); n != "" && n != "?" {
			seen[n] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
