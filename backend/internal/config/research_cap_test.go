package config_test

// 研究モードの「トリガー全採用」を **建玉の本数**の側から守るガード。
//
// ⚠ 「全採用」は無条件ではない。**監視銘柄の予算**(`account_max_open_symbols` /
// `per_entry_max_open_symbols`)があるので、**まだ持っていない銘柄**へのエントリーは枠で落ちうる
// (既に保有中の銘柄への重ね掛けは通る)。本数の cap は据え置き —— 同じ銘柄に複数アーム乗っても
// 監視集合は 1 銘柄なので、通信量と時価の解像度に効くのは銘柄数だけで、本数を絞る理由が無い。
// 銘柄側の整合は `symbol_budget_guard_test.go` が別に固定する。
//
// 1 銘柄に**戦略ごとの config が載る**ので、構造上の 1 日あたり上限は 銘柄数 × アーム数。
// `account_max_open_positions` がそれ以下だと cap が実際に効き、forward 標本が censoring される。
//
// 既存の `TestResearchConfigsUseTheDailyUniverseFile` は `> topN` しか見ていない。
// **アーム倍率を掛けるのはこのテストだけ**なので、メニューを増やしたら
// ここが落ちる(落ちたら config の cap を上げる — 下げてテストを通さない)。
//
// package config_test(外部テストパッケージ)なのは、arm 数の正本である
// `strategy.DefaultScreeners()` を import するため。strategy → config の依存があるので
// in-package テストから引くと import cycle になる。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/strategy"
)

func TestResearchAccountCapExceedsUniverseTimesArms(t *testing.T) {
	// ユニバース規則は「上限なし」(UNIV_TOP_N=0)。
	// 上限なしのときの銘柄側の天井はプール全体(hard_limits.allowed_symbols)。ただし監視銘柄の
	// 予算(account_max_open_symbols)が入っている config では、建玉を持てる銘柄はその予算で
	// 頭打ちなので、構造上の 1 日あたり上限 = **予算 × 回るアーム数**。
	universe := researchUniverseCeiling(t)

	for _, name := range []string{"bot_config.advisor.yaml"} {
		cfg, err := config.LoadBotConfig(researchConfigPath(t, name))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		// 回るアーム数: advisor 経路は `entries` で絞った screener 数、selector 経路は
		// 1 テンプレート = 1 アーム。
		arms := 1
		if cfg.Advisor.Enabled {
			active, err := strategy.ScreenersForEntries(cfg.Advisor.Entries)
			if err != nil {
				t.Fatalf("%s advisor_v2.entries: %v", name, err)
			}
			arms = len(active)
		}
		if arms <= 0 {
			t.Fatal("回るアーム数が 0 — メニューの正本(戦略カタログ)が壊れている")
		}
		symbols := universe
		if b := cfg.Risk.AccountMaxOpenSymbols; b > 0 && b < symbols {
			symbols = b
		}
		// 1 日で建ちうる上限。多日保有の繰越があるので、実際の commit 値はこれより余裕を持たせる。
		perDayMax := symbols * arms
		if cfg.Risk.AccountMaxOpenPositions <= perDayMax {
			t.Errorf("%s の account_max_open_positions = %d は 銘柄(%d) × アーム(%d) = %d 以下 — "+
				"1 銘柄に戦略ごとの config が載るので cap が実際に効き、forward 標本が censoring される",
				name, cfg.Risk.AccountMaxOpenPositions, symbols, arms, perDayMax)
		}
	}
}

func researchConfigPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "configs", name)
}

// 日次ユニバースの件数の天井。scripts/stockbot-routine.sh の UNIV_TOP_N が正本(選定規則が
// 実際に住んでいる場所)。**0 = 上限なし**のときはプール全体
// (configs/hard_limits.yaml の allowed_symbols)が天井。
func researchUniverseCeiling(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "stockbot-routine.sh"))
	if err != nil {
		t.Fatalf("read stockbot-routine.sh: %v", err)
	}
	m := regexp.MustCompile(`(?m)^UNIV_TOP_N=(\d+)$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("scripts/stockbot-routine.sh に UNIV_TOP_N が見つからない — 日次ユニバースの件数はここが正本")
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil || n < 0 {
		t.Fatalf("UNIV_TOP_N が読めない: %q", m[1])
	}
	if n > 0 {
		return n
	}
	hl, err := config.LoadHardLimits(researchConfigPath(t, "hard_limits.yaml"))
	if err != nil {
		t.Fatalf("load hard_limits.yaml: %v", err)
	}
	if len(hl.AllowedSymbols) == 0 {
		t.Fatal("hard_limits.allowed_symbols が空 — プールの天井が取れない")
	}
	return len(hl.AllowedSymbols)
}
