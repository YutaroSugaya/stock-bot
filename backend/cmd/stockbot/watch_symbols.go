package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

// watchedSymbols returns the symbols that need a running SymbolBundle:
// **今日のユニバース ∪ いま建玉のある銘柄**。
//
// 出口は全て bundle の中にある(価格ループ / TP・SL・max_hold・ratchet / 14:50 の
// 強制フラット化 / reconcile)のに、bundle は botCfg.Symbols のぶんしか作られない。
// 合流させないと、**ユニバースから外れた銘柄の建玉は誰にも決済されない**(人間が
// /api/flatten-all を押すまで)。紙ブローカーの OCO はレッグを記録するだけで自動
// 約定しないので paper でも同じ。日次ユニバースは毎朝入れ替わり、脱落するのは
// 「流動性が落ちた」「単元100万を超えた」= 損益と相関する事象なので、取り残しは
// forward 標本の非ランダムな欠測になる。
//
// 合流ぶんは呼び出し側で no_trade に固定する。順序はユニバースの並びを保ち、合流を
// **銘柄コード昇順**で末尾に足す(同じ状態から同じ監視対象が出ること = 再現性の前提)。
//
// 建玉一覧が引けないときは **fail-close**。「建玉が無いことにして起動する」と、その
// 建玉は管理外のまま残り続ける。
func watchedSymbols(ctx context.Context, repo port.PositionRepository, universe []string) (all, adopted []string, err error) {
	open, err := repo.ListOpenAllSymbols(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list open positions to adopt held symbols into the watch universe: %w (fail-close: 建玉を管理外にしたまま起動しない)", err)
	}
	inUniverse := make(map[string]bool, len(universe))
	for _, s := range universe {
		inUniverse[s] = true
	}
	extra := make(map[string]bool)
	for _, p := range open {
		if !inUniverse[p.Symbol] {
			extra[p.Symbol] = true
		}
	}
	adopted = make([]string, 0, len(extra))
	for s := range extra {
		adopted = append(adopted, s)
	}
	sort.Strings(adopted)

	all = make([]string, 0, len(universe)+len(adopted))
	all = append(all, universe...)
	all = append(all, adopted...)
	return all, adopted, nil
}

// logUniverseProvenance records WHICH day's universe this process is running.
//
// **bot は symbols_file を起動時に1回しか読まない。** 朝ジョブはファイルを書き換える
// だけで bot を再起動しないので、上がりっぱなしのプロセスは起動日の銘柄を使い続ける。
// 「毎朝選び直す」のは選定側で、適用は再起動時 — 混同すると、事前登録した規則と実際に
// 取った標本が食い違ったまま forward 記録が積み上がる。
func logUniverseProvenance(c *config.BotConfig, clk clock.Clock, logger *slog.Logger) {
	if c.SymbolsFile == "" {
		logger.Info("universe: 静的な symbols:(日次選定は使っていない)", "symbols", len(c.Symbols))
		return
	}
	path := c.SymbolsFile
	if p, err := config.ExpandHome(path); err == nil {
		path = p
	}
	fi, err := os.Stat(path)
	if err != nil {
		// 起動できている以上ファイルは読めたはずなので、ここに来るのは異常。
		logger.Warn("universe: symbols_file の情報を取得できない", "path", path, "err", err)
		return
	}
	now := clk()
	selectedOn := fi.ModTime().In(now.Location()).Format("2006-01-02")
	logger.Info("universe: 日次選定ファイルから読み込み(**反映は起動時の1回のみ**)",
		"path", path, "symbols", len(c.Symbols), "selected_on", selectedOn)
	if selectedOn != now.Format("2006-01-02") {
		logger.Warn("universe: 本日選定されたユニバースではない — 朝ジョブが失敗しているか、日付をまたいで起動しっぱなし。**forward 記録の銘柄集合はこの日付のもの**",
			"selected_on", selectedOn, "today", now.Format("2006-01-02"))
	}
}

// logStarted は起動完了の 1 行を出す。銘柄コードの一覧は出さず本数だけにする(523 銘柄で
// 1 行が数 KB になり端末を押し流していた。一覧は日次選定ファイルにある)。
// 🛑 msg の文言を変えない: scripts/db-restore.sh が最終稼働時刻をこの文言で探す。
func logStarted(logger *slog.Logger, mode config.Mode, broker config.BrokerKind, symbols []string,
	watched int, entries []config.StrategyName, arms int, live bool) {
	logger.Info("stockbot started", "mode", mode, "broker", broker,
		"universe", len(symbols), "watched", watched,
		"research_entries", entries, "research_arms", arms, "live_track", live)
}
