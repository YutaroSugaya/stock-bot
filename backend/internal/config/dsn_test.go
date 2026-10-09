package config

import "testing"

// 🚨 3 DB の分離判定が **DSN 文字列の完全一致**だけだと、`?sslmode=` の有無や
// `localhost` ↔ `127.0.0.1` の違いで**同一 DB を 2 トラックが同時に掴む**。
// 台帳の物理分離は A 案の全根拠なので、正規化して比べる。
func TestSameDatabaseDetectsEquivalentDSNs(t *testing.T) {
	same := [][2]string{
		{"postgres://u:p@localhost:5434/stockbot?sslmode=disable", "postgres://u:p@localhost:5434/stockbot"},
		{"postgres://u:p@localhost:5434/stockbot", "postgres://u:p@127.0.0.1:5434/stockbot"},
		{"postgres://u:p@localhost:5434/stockbot", "postgres://u:p@LOCALHOST:5434/stockbot"},
	}
	for _, c := range same {
		if !SameDatabase(c[0], c[1]) {
			t.Errorf("同じ DB を別物と判定した:\n  %s\n  %s", c[0], c[1])
		}
	}
	diff := [][2]string{
		{"postgres://u:p@localhost:5434/stockbot", "postgres://u:p@localhost:5434/stockbot_c3"},
		{"postgres://u:p@localhost:5434/stockbot", "postgres://u:p@localhost:5435/stockbot"},
	}
	for _, c := range diff {
		if SameDatabase(c[0], c[1]) {
			t.Errorf("別の DB を同一と判定した:\n  %s\n  %s", c[0], c[1])
		}
	}
	// 片方が空 = 比較対象なし。
	if SameDatabase("", "postgres://u:p@localhost/stockbot") {
		t.Error("空の DSN を同一と判定した")
	}
}

// 🛑 **解釈できない DSN は「同一」に倒す**(fail-close)。分離を証明できないものを
// 「別物だから安全」と読まない。以前は片方だけ解釈できるときに false を返しており、
// godoc の宣言と実装が食い違っていた。
func TestSameDatabaseFailsClosedOnUnparseableDSNs(t *testing.T) {
	cases := [][2]string{
		{"postgres://u:p@localhost:5434/stockbot", "not-a-dsn"},
		{"not-a-dsn", "postgres://u:p@localhost:5434/stockbot"},
		{"garbage-a", "garbage-b"},
	}
	for _, c := range cases {
		if !SameDatabase(c[0], c[1]) {
			t.Errorf("解釈できない DSN を「別 DB」と判定した(fail-open):\n  %s\n  %s", c[0], c[1])
		}
	}
	// 両方きちんと解釈できて別物なら false のまま。
	if SameDatabase("postgres://u:p@localhost:5434/stockbot", "postgres://u:p@localhost:5434/stockbot_c3") {
		t.Error("別 DB を同一と判定した")
	}
}
