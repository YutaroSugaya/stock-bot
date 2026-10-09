package main

import (
	"os"
	"path/filepath"
	"testing"

	"stockbot/backend/internal/domain/market"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestWriteUniverseFileWritesOnePerLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "today.txt")
	if err := writeUniverseFile(path, []string{"7203", "6758", "285A"}, 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := read(t, path), "7203\n6758\n285A\n"; got != want {
		t.Fatalf("content = %q, want %q(1行1銘柄・親ディレクトリは自動作成)", got, want)
	}
}

// 空を書くと bot は起動時に fail-close して上がらない(symbols_file が空 = 起動拒否)。
func TestWriteUniverseFileRefusesEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "today.txt")
	if err := os.WriteFile(path, []byte("7203\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeUniverseFile(path, nil, 0); err == nil {
		t.Fatal("空リストは書かずにエラーにすること")
	}
	if got := read(t, path); got != "7203\n" {
		t.Fatalf("失敗時に既存ファイルを壊した: %q", got)
	}
}

// 取得に失敗した日に数銘柄だけのユニバースで上書きすると「絞ったつもりの事故」になる。
func TestWriteUniverseFileEnforcesMinCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "today.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeUniverseFile(path, []string{"7203", "6758"}, 150); err == nil {
		t.Fatal("min-count を割ったら書かないこと")
	}
	if got := read(t, path); got != "old\n" {
		t.Fatalf("失敗時に既存ファイルを壊した: %q", got)
	}
	if err := writeUniverseFile(path, []string{"7203", "6758"}, 2); err != nil {
		t.Fatalf("ちょうど min-count なら書くこと: %v", err)
	}
}

func TestWriteUniverseFileIsAtomicAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "today.txt")
	if err := writeUniverseFile(path, []string{"1111", "2222", "3333"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := writeUniverseFile(path, []string{"9999"}, 0); err != nil {
		t.Fatal(err)
	}
	if got, want := read(t, path), "9999\n"; got != want {
		t.Fatalf("上書きが部分的: %q, want %q", got, want)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "today.txt" {
		t.Fatalf("temp ファイルが残っている: %v", ents)
	}
}

// 日次選定は allowed_symbols の内側の絞り込みでしかない(外の銘柄を残すと bot が
// 起動時に fail-close する)。
func TestFilterAllowedKeepsOnlyWhitelisted(t *testing.T) {
	u := map[string][]market.Candle{
		"7203": {{Close: 1}}, "6758": {{Close: 1}}, "1773": {{Close: 1}}, "25935": {{Close: 1}},
	}
	kept, dropped := filterAllowed(u, []string{"6758", "7203"})
	if len(kept) != 2 || kept["7203"] == nil || kept["6758"] == nil {
		t.Fatalf("ホワイトリスト内だけ残すこと: %v", keys(kept))
	}
	if len(dropped) != 2 || dropped[0] != "1773" || dropped[1] != "25935" {
		t.Fatalf("落とした銘柄をコード昇順で報告すること: %v", dropped)
	}
}

func TestFilterAllowedWithEmptyWhitelistKeepsNothing(t *testing.T) {
	u := map[string][]market.Candle{"7203": {{Close: 1}}}
	kept, dropped := filterAllowed(u, nil)
	if len(kept) != 0 || len(dropped) != 1 {
		t.Fatalf("空のホワイトリストは全部落とす(fail-close): kept=%v dropped=%v", keys(kept), dropped)
	}
}

// today.txt だけだと翌朝上書きされ、入力 CSV も30世代で流れるので、過去日の
// ユニバースを事後に再構成できなくなる。
func TestArchiveUniverseFileKeepsADatedCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "today.txt")
	if err := writeUniverseFile(path, []string{"7203", "6758"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := archiveUniverseFile(path, filepath.Join(dir, "archive"), "2026-08-12"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if got, want := read(t, filepath.Join(dir, "archive", "2026-08-12.txt")), "7203\n6758\n"; got != want {
		t.Fatalf("日付つきの控え = %q, want %q", got, want)
	}
	if got := read(t, path); got != "7203\n6758\n" {
		t.Fatalf("today.txt は残すこと: %q", got)
	}
}

func TestArchiveUniverseFileOverwritesSameDay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "today.txt")
	for _, syms := range [][]string{{"7203"}, {"7203", "6758"}} {
		if err := writeUniverseFile(path, syms, 0); err != nil {
			t.Fatal(err)
		}
		if err := archiveUniverseFile(path, filepath.Join(dir, "archive"), "2026-08-12"); err != nil {
			t.Fatalf("archive: %v", err)
		}
	}
	if got, want := read(t, filepath.Join(dir, "archive", "2026-08-12.txt")), "7203\n6758\n"; got != want {
		t.Fatalf("同日の再実行で最新に更新されること: %q, want %q", got, want)
	}
}

func keys(m map[string][]market.Candle) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// -out の前提: `-top-n 0`(上限なし)が正規の値になったので、
// 「-out には -top-n > 0 が必須」は外す。代わりに安全弁 `-min-count > 0` を必須にする —
// 下限の無い -out は「絞ったつもりで絞れていない」ファイルを運用に載せる。負値は
// 呼び手側の範囲検査が先に落とす(ここでは見ない)。
func TestOutFlagsRequireTheMinCountSafetyValve(t *testing.T) {
	if err := validateOutFlags("today.txt", 0, 150); err != nil {
		t.Fatalf("上限なし + 安全弁あり は書けること: %v", err)
	}
	if err := validateOutFlags("today.txt", 200, 150); err != nil {
		t.Fatalf("上限あり + 安全弁あり は従来どおり書けること: %v", err)
	}
	if err := validateOutFlags("today.txt", 0, 0); err == nil {
		t.Fatal("安全弁の無い -out は拒否すること")
	}
	if err := validateOutFlags("", 0, 0); err != nil {
		t.Fatalf("-out 無しなら安全弁は要らない: %v", err)
	}
}
