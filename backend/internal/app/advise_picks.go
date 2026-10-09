package app

import (
	"sort"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

// 枠は**戦略ごとのラウンドロビン**で配る。全体 score 降順にしないのは、score の
// 定義が戦略ごとに違って比較できないため — abs_momentum は上限なし(1.3〜1.8)、
// donchian_breakout は構造上 1.0 付近が上限なので、降順で上位 N を取ると
// abs_momentum が全枠を独占する(実測: LLM の選択 80 件中 73 件が
// abs_momentum、19 銘柄トリガーしたドンチャンは 0 件 = audition 不成立)。
// isHeld(建玉中・nil 可)は枠を消費しない — 凍結 config で走っているので再 arm しない。
// 🛑 isHeld のキーは **(銘柄, 戦略)**。銘柄だけで見ると、片方のアームが建った
// 瞬間にもう片方が永久に arm されなくなる(bnf ペアが 0 本になる原因の 1 つ)。
//
// rotate は巡回の先頭ずらし(呼び出し側は「その日」を渡す)。入口が同一で出口だけ
// 違う2アーム(bnf_reversion / bnf_reversion_trail)が1銘柄を取り合うとき、順序を
// 名前昇順で固定すると常に同じアームが勝ち、もう一方の標本が永久にゼロになる。
// 乱数は使わない — 同じ日・同じ入力なら必ず同じ結果になること(再現性)が要る。
//
// 枠を配る前に「出せない売り」を落とす。
//
// 🚨 なぜ枠の**前**か: 貸借銘柄フィルタ
// (risk.EvaluateShortLoanable)は発注時のゲートなので、貸借でない銘柄の売り候補も
// 一度 arm され、`per_strategy_n` の枠を 1 つ潰したまま**建玉にならずに日中居座る**。
// その戦略の下位にいる**買い候補が arm されなくなる** = 事前登録した
// 「向きを開けても買い側の測定には触れない」が破れる。
//
// 🛑 shortAllowed == nil は**従来どおり全部通す**。ここを fail-close に倒すと、
// 貸借と無関係な buy_only の戦略まで巻き添えで止まる。売りの fail-close は
// 発注前ゲートが持っている(こちらは枠の公平性のためだけの間引き)。
func selectAdvisePicks(ranked []strategy.Candidate, isHeld func(string, config.StrategyName) bool,
	totalCap, perStrategy, rotate int, shortAllowed func(symbol string) bool) []strategy.Candidate {
	if shortAllowed != nil {
		kept := make([]strategy.Candidate, 0, len(ranked))
		for _, c := range ranked {
			if c.Side == order.SideSell && !shortAllowed(c.Symbol) {
				continue
			}
			kept = append(kept, c)
		}
		ranked = kept
	}
	return selectAdvisePicksCore(ranked, isHeld, totalCap, perStrategy, rotate)
}

func selectAdvisePicksCore(ranked []strategy.Candidate, isHeld func(string, config.StrategyName) bool, totalCap, perStrategy, rotate int) []strategy.Candidate {
	if totalCap <= 0 {
		totalCap = 1
	}
	if perStrategy <= 0 {
		perStrategy = 1
	}
	byStrategy := map[config.StrategyName][]strategy.Candidate{}
	for _, c := range ranked {
		if !c.Triggered {
			continue
		}
		if isHeld != nil && isHeld(c.Symbol, c.Strategy) {
			continue
		}
		byStrategy[c.Strategy] = append(byStrategy[c.Strategy], c)
	}
	// 巡回順は名前昇順で固定。score 順にすると「常に同じ戦略が先」に戻って再び偏る。
	names := make([]string, 0, len(byStrategy))
	for st := range byStrategy {
		names = append(names, string(st))
	}
	sort.Strings(names)
	if n := len(names); n > 0 {
		r := ((rotate % n) + n) % n
		names = append(names[r:], names[:r]...)
	}

	out := make([]strategy.Candidate, 0, totalCap)
	// 🛑 dedup のキーは **(銘柄, 戦略)**。銘柄だけで dedup していたので、入口が
	// 同一で出口だけ違う 2 アームは**同じラウンドで両方 pick されることが構造的に
	// 不可能**だった(bnf の trigger は 118本/11銘柄で完全一致なのに picked は 31 vs 10、
	// ペア 0 本)。同一 (銘柄, 戦略) の重複は従来どおり 1 回に潰す。
	seen := make(map[string]bool, totalCap)
	for k := 0; k < perStrategy && len(out) < totalCap; k++ {
		for _, name := range names {
			if len(out) >= totalCap {
				break
			}
			// この戦略の k 番目「以降」で、まだ採られていない銘柄を1つ取る。
			// 重複で飛ばされた分を詰めるので、戦略の枠が他戦略との被りで痩せない。
			for _, c := range byStrategy[config.StrategyName(name)] {
				key := c.Symbol + "|" + string(c.Strategy)
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// shortAllowedOrNil は AdvisorLoop.ShortAllowed に渡す判定器。
//
// 🛑 **一覧が未 commit(空)なら nil を返す = 間引かない。** 枠の前で全 SELL を落とすと、
// 発注前ゲート(risk.EvaluateShortLoanable)まで届かず **`loanable_list_missing` が
// signal_rejections に 1 行も残らなくなる**。事前登録はその行を「売り標本がゼロなのは
// 一覧が無いからだ」と後から判るための唯一の手掛かりとして設計している
// (「production では一度も記録されない」配線バグは実際に起きる形)。
//
// 一覧が無いのは**起動時の設定漏れ**であって、静かに最適化して隠す状態ではない。
// 間引き(枠の公平性)は一覧がある前提でのみ意味を持つ。
func ShortAllowedOrNil(hl *config.HardLimits) func(string) bool {
	if hl == nil || len(hl.LoanableSymbols) == 0 {
		return nil
	}
	return hl.AllowsShortSymbol
}
