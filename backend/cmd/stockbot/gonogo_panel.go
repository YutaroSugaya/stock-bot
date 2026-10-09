package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	adapter "stockbot/backend/internal/adapter/gonogo"
	"stockbot/backend/internal/app"
	"stockbot/backend/internal/app/gonogo"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/usecase/query"
)

// gonogoDir は寄り前の銘柄判定のファイルの置き場(cmd/gonogo が書く)。
func gonogoDir() string {
	if v := os.Getenv("STOCKBOT_GONOGO_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".stockbot", "gonogo")
}

// isPaperGoNoGoFamily は paper で判定する入口(bnf 家族・兄弟込み)か。
func isPaperGoNoGoFamily(n config.StrategyName) bool {
	for _, f := range gonogo.PaperFamily {
		if strategy.EntryArmOf(n) == f {
			return true
		}
	}
	return false
}

// gonogoMissing は research と live の画面が見た「arm 済みなのに判定が無い銘柄」(「未判定を判定」が判定する集合)。
var gonogoMissing = &app.GoNoGoMissing{}

// gonogoRun は画面の「未判定を判定」ボタン(POST /api/gonogo/run)。両トラックの未判定を `gonogo -symbols` で
// 別プロセスに判定させて即 return する。**表示と記録だけ** — 結果は判定ファイルに書かれ、bot は画面に出すだけ。
// 15:30 以降・休場日・日足が古い日は gonogo 自身が何もせず終わる。
func gonogoRun() ([]string, error) {
	syms := gonogoMissing.All()
	if len(syms) == 0 {
		return nil, nil
	}
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".stockbot")
	l := &adapter.Launcher{Bin: filepath.Join(base, "bin", "gonogo"), LogPath: filepath.Join(base, "logs", "gonogo.log"),
		LockPath: filepath.Join(base, "run", "gonogo.lock")}
	if err := l.Launch(syms); err != nil {
		return nil, err
	}
	return syms, nil
}

// newGoNoGoPanel は dashboard の `gonogo`(今日の判定・銘柄で行と結合する)を組む。**表示だけ** —
// bot の発注経路は判定を読まない。読めない・無いときは「未判定」になるだけで bot は落ちない。
// include はそのトラックで判定する戦略(nil = 全部)。arm 済みなのに判定が無い銘柄はログに出し、
// 「未判定を判定」ボタンの集合(gonogoMissing)に置く。
func newGoNoGoPanel(track string, include func(config.StrategyName) bool, clk clock.Clock, logger *slog.Logger) func([]app.RankingGroup) query.GoNoGoView {
	q := query.NewListGoNoGo(&adapter.Journal{Dir: gonogoDir()}, clk)
	gap := &app.GoNoGoGapLog{Logger: logger, Track: track, Missing: gonogoMissing}
	return func(groups []app.RankingGroup) query.GoNoGoView {
		v := q.Execute(context.Background(), app.ArmedRankedSymbols(groups, include))
		gap.Observe(v.MissingArmed)
		return v
	}
}
