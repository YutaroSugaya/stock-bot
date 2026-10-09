package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"stockbot/backend/internal/domain/market"
)

// validateOutFlags は -out を書いてよい組み合わせかを決める。
//
// 「-out には -top-n > 0 が必須」という条件は置かない。選定規則は
// **上限なし**(UNIV_TOP_N=0・単元 ≤ 200 万 かつ 売買代金中央値 ≥ 10 億)なので、この条件が
// 残っていると毎朝の選定が拒否され、bot は**前日(旧規則・200 銘柄)のユニバース**のまま
// 走り続ける(黙って旧規則に縮退する = 一番気づきにくい壊れ方)。
// 代わりに安全弁 `-min-count > 0` を必須にする — 下限の無い -out は「絞ったつもりで
// 絞れていない」ファイルを運用に載せる。負値は呼び手の範囲検査が先に落とす。
func validateOutFlags(out string, topN, minCount int) error {
	if out == "" {
		return nil
	}
	if minCount <= 0 {
		return fmt.Errorf("-out には -min-count (>0) が必須です(下限の無いユニバースは書かない・-top-n %d)", topN)
	}
	return nil
}

// writeUniverseFile writes the selected symbols one per line, ATOMICALLY
// (temp → rename in the same directory).
//
// 失敗時は既存ファイルに一切触れない。朝の選定が失敗した日は「前日のユニバースで
// 動く」が正解で、空/半端なファイルを置くと bot 側の symbols_file が fail-close して
// 起動しない。そのため ①空リストは書かない ②minCount 未満は書かない の二重の下限。
func writeUniverseFile(path string, symbols []string, minCount int) error {
	if len(symbols) == 0 {
		return fmt.Errorf("選定結果が0銘柄: %s は書き換えない(前日のユニバースを残す)", path)
	}
	if minCount > 0 && len(symbols) < minCount {
		return fmt.Errorf("選定結果が %d 銘柄 < -min-count %d: %s は書き換えない(データ欠損の疑い)",
			len(symbols), minCount, path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename が成功していれば no-op

	if _, err := tmp.WriteString(strings.Join(symbols, "\n") + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp %s: %w", tmpName, err)
	}
	// fsync してから rename。落ちても古い/新しい完全なファイルのどちらかしか観測されない。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod temp %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// archiveUniverseFile copies the just-written universe to <dir>/<day>.txt so the
// day's selection survives tomorrow's overwrite.
//
// 選定は入力 CSV の状態に依存し、その CSV の退避は30世代しかない。出力を残さないと
// 30営業日より前のユニバースは規則から再構成できず、台帳の「同じ規則を回せば再現
// できる」が実行不能な指示になる。
func archiveUniverseFile(path, dir, day string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	dst := filepath.Join(dir, day+".txt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// filterAllowed drops every candidate that is not on the hard_limits whitelist,
// BEFORE ranking — the daily universe is an operational narrowing INSIDE
// allowed_symbols, never a way out of it.
//
// 選定側でも落とすのは、データディレクトリがホワイトリストと独立に育つから
// (fetch-daily は dir を glob する)。外の銘柄が選ばれると bot は起動時に fail-close
// する — 正しいが気付くのが朝になる。空のホワイトリストは全部落とす(fail-close)。
func filterAllowed(u map[string][]market.Candle, allowed []string) (map[string][]market.Candle, []string) {
	allow := make(map[string]bool, len(allowed))
	for _, s := range allowed {
		allow[s] = true
	}
	kept := make(map[string][]market.Candle, len(u))
	dropped := make([]string, 0)
	for sym, cs := range u {
		if allow[sym] {
			kept[sym] = cs
			continue
		}
		dropped = append(dropped, sym)
	}
	sort.Strings(dropped) // 決定論(map の反復順をログに出さない)
	return kept, dropped
}
