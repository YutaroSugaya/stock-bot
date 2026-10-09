package config

import (
	"net/url"
	"strings"
)

// SameDatabase は 2 つの DSN が**同じ database を指すか**。
//
// 🚨 文字列の完全一致だけで比べていたため、`?sslmode=disable` の有無や
// `localhost` ↔ `127.0.0.1` の違いで**同一 DB を 2 トラックが同時に掴んでも
// すり抜けた**。台帳の物理分離はサイクル境界の全根拠なので、
// ホスト名・ポート・DB 名だけを正規化して比べる。
//
// 🛑 解釈できない DSN は **同一と見なす**(fail-close)。分離を証明できないものを
// 「別物だから安全」と読まない。
func SameDatabase(a, b string) bool {
	if a == "" || b == "" {
		return false // 比較対象が無い(未設定はここでは扱わない)
	}
	ka, oka := dsnIdentity(a)
	kb, okb := dsnIdentity(b)
	// 🛑 **片方でも解釈できなければ「同一」に倒す**(fail-close)。以前は
	// `a == b || !oka && !okb` だったので、片方だけ解釈できるときに false(= 別 DB)を
	// 返し、godoc が謳う fail-close が効いていなかった。
	// 分離を証明できないものを「別物だから安全」と読まない。
	if !oka || !okb {
		return true
	}
	return ka == kb
}

// dsnIdentity は (host, port, database) を正規化した鍵。
func dsnIdentity(dsn string) (string, bool) {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "127.0.0.1" || host == "::1" || host == "localhost" {
		host = "localhost"
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	return host + ":" + port + "/" + strings.TrimPrefix(u.Path, "/"), true
}
