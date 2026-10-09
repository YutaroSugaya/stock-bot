package config

import "testing"

// arm の config を何で作るか(`advisor_v2.arm_source`)。空 = template(LLM は既定 OFF —
// CLAUDE.md §4)。LLM 経路に戻すのは `arm_source: llm` と明示したときだけ。
func TestArmSource(t *testing.T) {
	for _, c := range []struct {
		yaml    string
		wantLLM bool
	}{
		{"mode: paper_config\nadvisor_v2:\n  enabled: true\n", false},
		{"mode: paper_config\nadvisor_v2:\n  enabled: true\n  arm_source: template\n", false},
		{"mode: paper_config\nadvisor_v2:\n  enabled: true\n  arm_source: llm\n", true},
	} {
		cfg, err := LoadBotConfig(writeTemp(t, "bot.yaml", c.yaml))
		if err != nil {
			t.Fatalf("%q: %v", c.yaml, err)
		}
		if got := cfg.Advisor.UsesLLM(); got != c.wantLLM {
			t.Errorf("%q: UsesLLM = %v, want %v", c.yaml, got, c.wantLLM)
		}
	}
}

// 🛑 綴りの誤りと旧キー(`deterministic`)は起動時 error。黙って既定に落とすと、
// 書いた人の意図と違う経路で arm が回る。
func TestArmSourceRejectsUnknownValuesAndTheOldKey(t *testing.T) {
	for _, y := range []string{
		"mode: paper_config\nadvisor_v2:\n  arm_source: LLM\n",
		"mode: paper_config\nadvisor_v2:\n  deterministic: true\n",
	} {
		if _, err := LoadBotConfig(writeTemp(t, "bot.yaml", y)); err == nil {
			t.Errorf("%q を通した", y)
		}
	}
}
