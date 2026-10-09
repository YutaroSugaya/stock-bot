package advisor

import (
	"os"
	"path/filepath"
)

// DefaultOutputDir は LLM 経路の生 stdout の監査アーカイブの既定の置き場所(~/.stockbot/ai_output)。
// 相対パスにすると cwd=backend で起動したとき repo の中に落ちる。HOME が引けなければ一時ディレクトリ。
func DefaultOutputDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "stockbot-ai-output")
	}
	return filepath.Join(home, ".stockbot", "ai_output")
}
