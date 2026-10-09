package main

import (
	"context"
	"fmt"
	"sort"

	"stockbot/backend/internal/port"
)

// candleCoverageSymbols returns the symbols whose daily bars must be kept fresh
// in the candle repo: **今日のユニバース ∪ いま建玉のある銘柄(全トラック)**。
//
// 🚨 なぜ「ユニバースだけ」では足りないか: seed / refresh は botCfg.Symbols しか
// 見ていなかったので、ユニバースから外れた保有銘柄の日足が**その日で凍結**した。
// bundle 側は建玉銘柄を監視対象に合流させる(watchedSymbols)のに、日足の取り込みは
// 合流しない —— この非対称が穴だった。3 営業日
// (app.maxDailyCandleAgeTradingDays)を超えると bundle が fail-close して
// `daily candles stale; skipping day-horizon eval` を吐き、day-horizon の評価が
// 止まる。CSV は朝ジョブが更新し続けているので、**読みに行っていないだけ**という
// 気づきにくい形で出る。
//
// 🛑 建玉は **トラックごとに別 DB**(research / live)なのに日足の repo は
// 共有している。1 つの repo だけ見ると live だけが持つ銘柄を取りこぼすので
// 可変長で受ける。nil の repo(env 未設定 / 起動途中でまだ開いていない)は飛ばす。
//
// 順序は watchedSymbols と揃える: ユニバースの並びを保ち、合流ぶんを**銘柄コード
// 昇順**で末尾に足す(同じ状態から同じ集合が出ること = 再現性の前提)。
//
// 建玉一覧が引けないときは **error を返す**。黙ってユニバースだけに縮退すると、
// 取り込み対象が減ったことが誰にも見えないまま凍結バグが再来する。縮退するかは
// 呼び出し側が warn を出したうえで決める。
func candleCoverageSymbols(ctx context.Context, universe []string, repos ...port.PositionRepository) ([]string, error) {
	inUniverse := make(map[string]bool, len(universe))
	for _, s := range universe {
		inUniverse[s] = true
	}
	extra := make(map[string]bool)
	for _, repo := range repos {
		if repo == nil {
			continue
		}
		open, err := repo.ListOpenAllSymbols(ctx)
		if err != nil {
			return nil, fmt.Errorf("list open positions to keep their daily bars fresh: %w", err)
		}
		for _, p := range open {
			if !inUniverse[p.Symbol] {
				extra[p.Symbol] = true
			}
		}
	}
	adopted := make([]string, 0, len(extra))
	for s := range extra {
		adopted = append(adopted, s)
	}
	sort.Strings(adopted)

	all := make([]string, 0, len(universe)+len(adopted))
	all = append(all, universe...)
	return append(all, adopted...), nil
}

// candleSymbols is the runtime-wide accessor for candleCoverageSymbols.
//
// live は env 未設定なら nil、**起動途中(openStore 直後の seed)ではまだ
// 開いていない**ので nil で渡る。その段階では research のぶんだけが合流し、
// 両トラックが揃うのは startBackgroundJobs 以降(そこで走る
// refreshDailyCandles が残りを拾う)。
//
// 引けなかったときはユニバースだけに縮退するが、**必ず warn を残す** — 黙って
// 縮退すると「day-horizon が止まっているのに誰も気づかない」元の状態に戻る。
func (r *runtimeState) candleSymbols(ctx context.Context) []string {
	repos := []port.PositionRepository{r.st.positions}
	if r.live != nil {
		repos = append(repos, r.live.store.positions)
	}
	syms, err := candleCoverageSymbols(ctx, r.botCfg.Symbols, repos...)
	if err != nil {
		r.logger.Warn("日足の取り込み対象に建玉銘柄を合流できない — ユニバースだけで続行する"+
			"(ユニバース外の建玉は 3 営業日で day-horizon 評価が止まる)", "err", err)
		return r.botCfg.Symbols
	}
	return syms
}
