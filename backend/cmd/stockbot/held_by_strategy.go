package main

import (
	"context"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/port"
)

// heldByStrategy は「その (銘柄, 戦略) が建玉中か」。
//
// 🛑 **戦略不明(`strategy_name == ""`)の建玉は全戦略を建玉中とみなす**。external
// (人間が証券アプリで建てた)と、backfill が届かなかった旧建玉が該当する。
// 「戦略が違う」と読むと**人間の建玉と二重に持つ**。ナンピン禁止ゲートが snapshot 側で
// 同じ倒し方をするので、arm 側だけ緩めると「arm したのに毎ティック reject される」
// 銘柄が生まれる。
//
// 🚨 **台帳が読めないときは true(= 建玉中とみなす)**。以前は main.go の
// インライン閉包で `return false` に落ちており、①テストから到達できず ②DB 不調のときに
// **建玉中の銘柄を「空いている」と読んで arm する**形だった。ナンピン禁止の hard gate が
// 発注前に fail-close するので二重建玉には至らないが、arm 枠を空転させる。
// 分からないなら建てない側へ倒す。
func heldByStrategy(repo port.PositionRepository) func(context.Context, string, config.StrategyName) bool {
	return func(ctx context.Context, symbol string, name config.StrategyName) bool {
		open, err := repo.ListOpenOrClosing(ctx, symbol)
		if err != nil {
			return true // 判定できない = 建てない側(fail-close)
		}
		for _, p := range open {
			if p.StrategyName == "" || p.StrategyName == string(name) {
				return true
			}
		}
		return false
	}
}
