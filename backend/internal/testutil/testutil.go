// Package testutil holds shared test helpers (TESTING.md §共有ヘルパ)。
// production コードからは import しない(テスト専用)。古典派 TDD の real collaborator
// 方針を崩さないよう、ここに置くのは「境界の値比較 / ログ抑止 / 一時ファイル」など
// アサーションの足回りだけで、fake/mock のたぐいは置かない。
//
// 寄せるのは**綴りがずれると意味が変わる重複だけ**。`clock.Fixed` / `math.Abs` の
// 直呼びはラップしない(1 行のラッパを挟む利点が無い — RF2 / RF3 #12)。
package testutil

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

// SilentLogger はテスト出力を汚さない discard logger を返す。
func SilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TempFlagPath は emergency_stop フラグ用の一時パスを t.TempDir() 配下に作って返す。
// 実ファイルで safety.EmergencyStop の Trip/Active/Resume を検証するときに使う。
func TempFlagPath(t testing.TB) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "emergency_stop.flag")
}
