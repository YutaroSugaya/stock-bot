package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 黙って静的リストに落ちると「ユニバースを絞ったつもりで絞れていない」事故になる
// (max_position_notional_jpy が selector 経路にしか無く、走っている
// advisor 経路では効いていなかったのと同じ形)。

func writeTmp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveSymbolsFileOverridesStaticList(t *testing.T) {
	p := writeTmp(t, "today.txt", "7203\n6758\n285A\n")
	c := &BotConfig{Symbols: []string{"9999"}, SymbolsFile: p}
	if err := c.ResolveSymbols(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := strings.Join(c.Symbols, ","); got != "7203,6758,285A" {
		t.Fatalf("symbols = %q, want 7203,6758,285A(ファイルが symbols: を上書き)", got)
	}
}

// 空行・コメント・前後の空白(CRLF 混じりを含む)は無視する。人間が朝の選定結果を
// 手で1銘柄外す運用が起きうるので、そこで壊れない形にしておく。
func TestResolveSymbolsFileIgnoresBlanksAndComments(t *testing.T) {
	p := writeTmp(t, "today.txt", "# 2026-08-10 selection\r\n\n 7203 \r\n\n# 6758 を今日は外す\n8306\n")
	c := &BotConfig{SymbolsFile: p}
	if err := c.ResolveSymbols(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := strings.Join(c.Symbols, ","); got != "7203,8306" {
		t.Fatalf("symbols = %q, want 7203,8306", got)
	}
}

// 読めない = fail-close。静的リストへの縮退は**しない**。
func TestResolveSymbolsFileMissingFailsClosed(t *testing.T) {
	c := &BotConfig{Symbols: []string{"7203"}, SymbolsFile: filepath.Join(t.TempDir(), "nope.txt")}
	err := c.ResolveSymbols()
	if err == nil {
		t.Fatal("読めない symbols_file は起動拒否にすること(静的リストに落ちない)")
	}
	if !strings.Contains(err.Error(), "symbols_file") {
		t.Fatalf("エラーに symbols_file の文脈が要る: %v", err)
	}
}

func TestResolveSymbolsFileEmptyFailsClosed(t *testing.T) {
	for _, content := range []string{"", "\n\n", "# 全部コメント\n"} {
		c := &BotConfig{Symbols: []string{"7203"}, SymbolsFile: writeTmp(t, "today.txt", content)}
		if err := c.ResolveSymbols(); err == nil {
			t.Fatalf("空の symbols_file(%q)は起動拒否にすること", content)
		}
	}
}

// 同じ銘柄が2行あると SymbolBundle が二重に立つ(同一銘柄に価格ループが2本)。
// ナンピン禁止ゲートが救ってくれるとしても、二重配線そのものを許さない。
func TestResolveSymbolsFileRejectsDuplicates(t *testing.T) {
	c := &BotConfig{SymbolsFile: writeTmp(t, "today.txt", "7203\n6758\n7203\n")}
	if err := c.ResolveSymbols(); err == nil {
		t.Fatal("重複銘柄は拒否すること(SymbolBundle が二重に立つ)")
	}
}

// 静的な symbols: 側の重複も同じ理由で拒否する。
func TestValidateRejectsDuplicateStaticSymbols(t *testing.T) {
	hl := &HardLimits{AllowedSymbols: []string{"7203", "6758"}}
	c := &BotConfig{Mode: ModePaper, Symbols: []string{"7203", "6758", "7203"}}
	if err := c.ValidateAgainstHardLimits(hl); err == nil {
		t.Fatal("symbols: の重複も拒否すること")
	}
}

// 先頭 ~ はホームに展開する(committed config をマシン非依存に保つため)。
func TestResolveSymbolsFileExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("home 不明: %v", err)
	}
	rel, err := filepath.Rel(home, writeTmp(t, "today.txt", "7203\n"))
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Skip("t.TempDir がホーム配下でないため ~ 展開は実ファイルで確認できない")
	}
	c := &BotConfig{SymbolsFile: "~/" + rel}
	if err := c.ResolveSymbols(); err != nil {
		t.Fatalf("~ 展開に失敗: %v", err)
	}
	if len(c.Symbols) != 1 || c.Symbols[0] != "7203" {
		t.Fatalf("symbols = %v", c.Symbols)
	}
}

// ResolveSymbols を呼び忘れると「日次ユニバースを設定したのに static な symbols: で
// 走る」事故が静かに起きる。起動経路で必ず通る validate に閂を掛けておく。
func TestValidateRequiresSymbolsFileToBeResolved(t *testing.T) {
	hl := &HardLimits{AllowedSymbols: []string{"7203"}}
	c := &BotConfig{Mode: ModePaper, Symbols: []string{"7203"}, SymbolsFile: "/tmp/whatever.txt"}
	if err := c.ValidateAgainstHardLimits(hl); err == nil {
		t.Fatal("symbols_file を解決せずに validate を通してはいけない")
	}
	c2 := &BotConfig{Mode: ModePaper, SymbolsFile: writeTmp(t, "today.txt", "7203\n")}
	if err := c2.ResolveSymbols(); err != nil {
		t.Fatal(err)
	}
	if err := c2.ValidateAgainstHardLimits(hl); err != nil {
		t.Fatalf("解決済みなら通ること: %v", err)
	}
}

// symbols_file 側に allowed_symbols 外の銘柄が来たら起動拒否(既存の fail-close を
// そのまま通す)。毎朝の選定は allowed_symbols の**内側**の絞り込みでしかない。
func TestValidateRejectsSymbolsFileOutsideWhitelist(t *testing.T) {
	hl := &HardLimits{AllowedSymbols: []string{"7203"}}
	c := &BotConfig{Mode: ModePaper, SymbolsFile: writeTmp(t, "today.txt", "7203\n9999\n")}
	if err := c.ResolveSymbols(); err != nil {
		t.Fatal(err)
	}
	err := c.ValidateAgainstHardLimits(hl)
	if err == nil || !strings.Contains(err.Error(), "9999") {
		t.Fatalf("allowed_symbols 外を含むファイルは起動拒否: %v", err)
	}
}

// symbols_file 未設定なら従来どおり(解決は no-op で、static リストで走る)。
func TestResolveSymbolsWithoutFileKeepsStaticList(t *testing.T) {
	c := &BotConfig{Mode: ModePaper, Symbols: []string{"7203"}}
	if err := c.ResolveSymbols(); err != nil {
		t.Fatalf("symbols_file 未設定は no-op: %v", err)
	}
	if len(c.Symbols) != 1 || c.Symbols[0] != "7203" {
		t.Fatalf("symbols = %v", c.Symbols)
	}
	if err := c.ValidateAgainstHardLimits(&HardLimits{AllowedSymbols: []string{"7203"}}); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// LoadBotConfig は symbols_file を読まない。読み込みが外部ファイルの存在に依存すると、
// config を読むだけのテストやツールが実行環境に縛られる。
func TestLoadBotConfigDoesNotTouchSymbolsFile(t *testing.T) {
	p := writeTmp(t, "bot.yaml", "mode: paper_config\nsymbols_file: /does/not/exist.txt\nsymbols: [\"7203\"]\n")
	c, err := LoadBotConfig(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.SymbolsFile != "/does/not/exist.txt" {
		t.Fatalf("symbols_file = %q", c.SymbolsFile)
	}
	if len(c.Symbols) != 1 || c.Symbols[0] != "7203" {
		t.Fatalf("読み込み時点では symbols: のまま: %v", c.Symbols)
	}
}

// 🚨 07:00 の選定ファイルに、その後 allowed_symbols から外した 9508(上場廃止)が
// 残っていて、make start が「whitelist に無い」で起動を拒否した(寄り 7 分前)。選定ファイルは
// **生成物**で、正本(hard_limits)より古いことがありうる。外れた銘柄は**除外して起動する**
// (減る方向 = fail-safe)。人間が書いた静的な symbols: は従来どおり起動拒否。
func TestDropSymbolsOutsideWhitelist_FromSymbolsFile(t *testing.T) {
	p := writeTmp(t, "today.txt", "7203\n9508\n6758\n")
	c := &BotConfig{SymbolsFile: p}
	if err := c.ResolveSymbols(); err != nil {
		t.Fatal(err)
	}
	hl := &HardLimits{AllowedSymbols: []string{"7203", "6758"}}
	dropped := c.DropSymbolsOutsideWhitelist(hl)
	if strings.Join(dropped, ",") != "9508" {
		t.Fatalf("dropped = %v, want [9508]", dropped)
	}
	if got := strings.Join(c.Symbols, ","); got != "7203,6758" {
		t.Fatalf("symbols = %q, want 7203,6758", got)
	}
	if err := c.ValidateAgainstHardLimits(hl); err != nil {
		t.Fatalf("除外後も起動拒否: %v", err)
	}
}

// 静的な symbols:(人間の設定)は除外しない — 打ち間違いを黙って消すと気づけない。
func TestDropSymbolsOutsideWhitelist_StaticListUntouched(t *testing.T) {
	c := &BotConfig{Symbols: []string{"7203", "9508"}}
	if err := c.ResolveSymbols(); err != nil {
		t.Fatal(err)
	}
	hl := &HardLimits{AllowedSymbols: []string{"7203"}}
	if dropped := c.DropSymbolsOutsideWhitelist(hl); len(dropped) != 0 {
		t.Fatalf("静的な symbols: から除外した: %v", dropped)
	}
	if err := c.ValidateAgainstHardLimits(hl); err == nil {
		t.Fatal("静的な symbols: の whitelist 外を通した")
	}
}

// 全部外れたら空 = 従来どおり起動拒否(空のユニバースで黙って走らない)。
func TestDropSymbolsOutsideWhitelist_AllDroppedStillFailsClosed(t *testing.T) {
	p := writeTmp(t, "today.txt", "9508\n")
	c := &BotConfig{SymbolsFile: p}
	if err := c.ResolveSymbols(); err != nil {
		t.Fatal(err)
	}
	hl := &HardLimits{AllowedSymbols: []string{"7203"}}
	c.DropSymbolsOutsideWhitelist(hl)
	if err := c.ValidateAgainstHardLimits(hl); err == nil {
		t.Fatal("空のユニバースで起動を許した")
	}
}
