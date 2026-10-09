package advisor

import (
	"path/filepath"
	"testing"
)

// 監査アーカイブの既定の出力先は ~/.stockbot/ai_output(絶対パス)。相対パスだと
// cwd=backend で起動したとき repo の中(backend/runtime/ai_output)に落ちる。
func TestDefaultOutputDirIsUnderStockbotHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := DefaultOutputDir()
	if want := filepath.Join(home, ".stockbot", "ai_output"); got != want {
		t.Fatalf("DefaultOutputDir = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("相対パス %q — cwd 次第で repo の中に落ちる", got)
	}
}
