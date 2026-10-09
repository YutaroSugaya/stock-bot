package strategy

import (
	"strings"

	"stockbot/backend/internal/config"
)

// EntryArmOf は兄弟アーム(`_trail`)を基のアームへ畳んだ **入口**の名前。
//
// メニューは「戦略が 12 個」ではなく **入口 7 / うち 5 に出口 2 通り = 12 アーム**
// (CLAUDE.md)。兄弟は**入口が完全に同一**で、同じトリガーの同じ銘柄に乗る。
//
// 🛑 枠を配る単位はアームではなく**入口**。アーム単位で数えると、枠の境界で
// **ペアの 2 本目だけが弾かれてペア差が構造的に測れなくなる**
// (「ペア 0 件」になる壊れ方そのもの)。MaxHold と direction が
// 既に同じ作法で畳んでいるのに、名前を持つ関数が無いせいで `strings.TrimSuffix` が
// 3 箇所へ写っていた。正本はここ。
func EntryArmOf(n config.StrategyName) config.StrategyName {
	return config.StrategyName(strings.TrimSuffix(string(n), TrailArmSuffix))
}

// SiblingArms は入口に属するアーム名(基 + `_trail`)を **基 → 兄弟の順**で返す。
// ペアを持たない入口(high_volume_premium / post_jump_drift)でも 2 要素返す —
// 存在しない名前は台帳に 1 行も無いので、数える側で害が無い(呼び手を分岐させると
// 「ペアがある入口だけ枠が 2 倍」という取り違えが必ず起きる)。
func SiblingArms(n config.StrategyName) []string {
	base := string(EntryArmOf(n))
	return []string{base, base + TrailArmSuffix}
}
