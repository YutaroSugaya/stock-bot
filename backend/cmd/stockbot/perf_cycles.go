package main

import (
	"context"
	"log/slog"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/app/handler"
	"stockbot/backend/internal/app/perfcycles"
)

// loadPerfCycles は画面の戦績のサイクル表(internal/app/perfcycles/cycles.yaml)。
// 読めなければ warn してサイクルを出さない(画面の戦績のためだけに起動を止めない)。
func loadPerfCycles(logger *slog.Logger) perfcycles.Table {
	tbl, err := perfcycles.Load()
	if err != nil {
		logger.Warn("戦績のサイクル表を読めない — サイクル選択を出さない", "err", err)
		return perfcycles.Table{}
	}
	return tbl
}

// researchPerfCycles は research トラックのサイクル一覧を組み、過去 DB のプールの解放を
// shutdown に繋ぐ。in-memory 構成(DSN なし)では現サイクルと「この台帳」の全期間だけになる。
func (r *runtimeState) researchPerfCycles(tbl perfcycles.Table) []handler.PerformanceCycle {
	if len(tbl.Cycles) == 0 {
		return nil
	}
	researchDB := dsnDatabaseName(r.env.DatabaseURL)
	sources, closeArchives := perfcycles.OpenArchives(context.Background(), r.env.DatabaseURL, researchDB, tbl.Cycles, r.logger)
	prev := r.onShutdown
	r.onShutdown = func() {
		if prev != nil {
			prev()
		}
		closeArchives()
	}
	if researchDB == "" {
		// in-memory: 現サイクルの行だけを既定の台帳に当てる。
		researchDB = tbl.Cycles[len(tbl.Cycles)-1].DB
	} else if r.st.trades != nil {
		sources[researchDB] = repository.TradeSource{Trades: r.st.trades, Strategies: r.st.strategies}
	}
	return perfcycles.Build(tbl.Cycles, sources, researchDB, perfReportOptions)
}
