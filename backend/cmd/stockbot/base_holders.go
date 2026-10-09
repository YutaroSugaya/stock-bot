package main

import "stockbot/backend/internal/app"

// baseHolders は ConfigSet の集合から「1 銘柄 1 config」のビューを作る。
//
// 🛑 決定論 Selector(live トラックと selector 経路)は **1銘柄1ポジのまま**。
// 同一銘柄のエクスポージャが最大でアーム数ぶんになる
// 緩和は paper(research)限定で、研究モードの設定を実弾に
// 持ち込まない前例(全トリガー採用 / account_max_open_positions)に揃える。
//
// base holder だけを渡すことで、Selector は arm 集合((銘柄, 戦略) 側)に
// 型として触れない。
func baseHolders(sets map[string]*app.ConfigSet) map[string]*app.ActiveConfigHolder {
	out := make(map[string]*app.ActiveConfigHolder, len(sets))
	for sym, cs := range sets {
		out[sym] = cs.Base()
	}
	return out
}
