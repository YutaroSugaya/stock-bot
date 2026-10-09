package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 🚨 **yaml の未知キーは起動時に拒否する**。
//
// それまで 3 つのローダは `yaml.Unmarshal` で、知らないキーを黙って捨てていた。実例は
// live の bot_config にあった `holding.default_mode: multiday` — 対応するフィールドが無く
// (保有モードの正は strategy_config の `holding_mode`)、書いた人には効いているように見えて
// 何もしていなかった。綴りを間違えた安全装置も同じ形で黙って消える。
func TestLoaders_RejectUnknownKeys(t *testing.T) {
	cases := []struct {
		name string
		load func(string) error
		body string
	}{
		{"bot_config", func(p string) error { _, err := LoadBotConfig(p); return err },
			"mode: paper_config\nholding:\n  default_mode: multiday\n"},
		{"hard_limits", func(p string) error { _, err := LoadHardLimits(p); return err },
			hardLimitsYAML + "no_such_limit: 1\n"},
		{"strategy_config", func(p string) error { _, err := LoadStrategyConfig(p); return err },
			"strategy_name: no_trade\nexit:\n  take_profit_jyp: 100\n"},
	}
	for _, c := range cases {
		err := c.load(writeTemp(t, c.name+".yaml", c.body))
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s: 未知キーを黙って捨てた / 理由が違う: %v", c.name, err)
		}
	}
}

// 空のファイルは従来どおり読める(Unmarshal は空を許していた。Decoder は io.EOF を返す)。
func TestLoaders_EmptyStrategyConfigStillLoads(t *testing.T) {
	c, err := LoadStrategyConfig(writeTemp(t, "empty.yaml", ""))
	if err != nil || c.StrategyName != StrategyNoTrade {
		t.Fatalf("空の strategy_config が読めない: %+v %v", c, err)
	}
}

// 🛑 **手元にある全 yaml を新しい decoder で読む**。未知キーを足した commit はここで落ちる。
// `~/.stockbot` の写し(launchd 用)も、あれば読む。
//
// git 管理外の live の 2 本(`bot_config.live.yaml` / `strategy_config.live.yaml`)は
// ここでは読まない — リポジトリの検査が作業者の手元の唯一のコピーに依存すると、
// 直すまで全員の `make check-backend` が赤くなる。あの 2 本は起動時に同じ decoder で
// 読まれ、未知キーがあれば起動が止まる(fail-close)。
func TestAllConfigYAMLDecodeStrictly(t *testing.T) {
	home, _ := os.UserHomeDir()
	var files []string
	for _, g := range []string{"../../../configs/*.yaml", "../../../configs/live/*.yaml", filepath.Join(home, ".stockbot", "*.yaml")} {
		m, err := filepath.Glob(g)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	gitignored := map[string]bool{"bot_config.live.yaml": true, "strategy_config.live.yaml": true}
	seen := 0
	for _, f := range files {
		base := filepath.Base(f)
		if gitignored[base] && strings.Contains(filepath.ToSlash(f), "/configs/") {
			continue
		}
		var err error
		switch {
		case strings.HasPrefix(base, "bot_config"):
			_, err = LoadBotConfig(f)
		case strings.HasPrefix(base, "hard_limits"):
			_, err = LoadHardLimits(f)
		case strings.HasPrefix(base, "strategy_config"):
			_, err = LoadStrategyConfig(f)
		default:
			t.Errorf("%s: どのローダで読むか分からない yaml(ここに足すこと)", f)
			continue
		}
		seen++
		if err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if seen < 5 {
		t.Fatalf("読んだ yaml が %d 本 — glob の起点がずれている", seen)
	}
}
