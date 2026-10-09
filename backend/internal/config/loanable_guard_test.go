package config

import (
	"os"
	"regexp"
	"testing"
	"time"
)

// 貸借ゲートの fail-close を出荷 config 側で固定する。
//
// 🛑 `loanable_symbols` が**空でも起動は通る**(それが fail-close の形 = 売りが 1 本も
// 建たないだけ)。だからこそ、キーが**存在すること**と、入っている値が
// `allowed_symbols` の内側であることをここで縛る — 貸借銘柄一覧を貼るときに
// 「プール外の銘柄を混ぜた」「5文字コードを貼った」を通さないため。
func TestLoanableSymbolsStayInsideTheAllowedPool(t *testing.T) {
	hl, err := LoadHardLimits(repoConfig(t, "hard_limits.yaml"))
	if err != nil {
		t.Fatalf("load hard_limits: %v", err)
	}
	allowed := make(map[string]bool, len(hl.AllowedSymbols))
	for _, s := range hl.AllowedSymbols {
		allowed[s] = true
	}
	seen := map[string]bool{}
	for _, s := range hl.LoanableSymbols {
		if !allowed[s] {
			t.Errorf("loanable_symbols の %q が allowed_symbols のプール外", s)
		}
		if seen[s] {
			t.Errorf("loanable_symbols に %q が重複している", s)
		}
		seen[s] = true
		if len(s) != 4 {
			t.Errorf("loanable_symbols の %q は4文字ではない", s)
		}
	}
}

// 空リストは「まだ人間が貸借銘柄一覧を commit していない」= 売り側の標本ゼロを意味する。
// **それ自体は正しい状態**なので落とさないが、`AllowsShortSymbol` が確かに fail-close
// していることは縛る(ここが true に倒れると、拒否される注文を毎ティック出し続ける)。
func TestAllowsShortSymbolFailsClosedOnAnEmptyList(t *testing.T) {
	empty := &HardLimits{AllowedSymbols: []string{"7203"}}
	if empty.AllowsShortSymbol("7203") {
		t.Fatal("loanable_symbols が空なのに売建を許した(fail-close でない)")
	}
	var nilHL *HardLimits
	if nilHL.AllowsShortSymbol("7203") {
		t.Fatal("nil hard_limits で売建を許した")
	}
	withList := &HardLimits{AllowedSymbols: []string{"7203"}, LoanableSymbols: []string{"7203"}}
	if !withList.AllowsShortSymbol("7203") {
		t.Fatal("一覧にある銘柄の売建が許可されない")
	}
	if withList.AllowsShortSymbol("6758") {
		t.Fatal("一覧に無い銘柄の売建が許可された")
	}
}

// 🛑 貸借銘柄一覧の**鮮度を機械で見るための構造化キー**を消させない。
//
// 停止条件 **S1(一覧が 60 暦日を超えて古い)** は、観測点が
// yaml の自由文コメントの生成日しか無く**検知器がゼロ**だった(S2 / S4 は
// 受入スクリプトにあるが S1 だけ取り残していた)。
//
// `stockbot-routine.sh` の expiry-manifest がこのキーを読んで 45 日 ⚠ / 60 日 🛑 で
// 警告する。**キーが消えると警告も黙って消える**(manifest の値が空になるだけで
// エラーにならない)ので、消えないことを Go 側で縛る。シェル側で「キーが無い」を
// 警告しようとすると、合成 repo を食わせる `stockbot-routine_test.sh` が正常系で
// 落ちる — 存在保証は yaml を読む側の責任で、シェルは日数計算だけを持つ。
func TestLoanableAsOfKeyExistsAndParses(t *testing.T) {
	raw, err := os.ReadFile(repoConfig(t, "hard_limits.yaml"))
	if err != nil {
		t.Fatalf("read hard_limits: %v", err)
	}
	re := regexp.MustCompile(`(?m)^[[:space:]]*loanable_symbols_as_of:[[:space:]]*"?(\d{4}-\d{2}-\d{2})"?`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatal("hard_limits.yaml に loanable_symbols_as_of が無い(または YYYY-MM-DD でない)" +
			" — これが消えると停止条件 S1 の鮮度警告が黙って止まる")
	}
	as, err := time.Parse("2006-01-02", string(m[1]))
	if err != nil {
		t.Fatalf("loanable_symbols_as_of が日付として読めない: %q", m[1])
	}
	// 未来日だと「常に新しい」になって S1 が永久に成立しない。
	if as.After(time.Now().AddDate(0, 0, 1)) {
		t.Errorf("loanable_symbols_as_of=%s が未来 — S1 が永久に成立しなくなる", m[1])
	}
}
