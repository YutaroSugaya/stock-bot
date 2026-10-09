package main

import (
	"context"
	"strings"
	"testing"

	"stockbot/backend/internal/adapter/repository"
)

// 日足の取り込み対象は **ユニバース ∪ 全トラックの建玉銘柄**。
//
// 🚨 なぜ要るか: seed / refresh は長らく botCfg.Symbols(= 今日のユニバース 200)しか
// 見ていなかった。ユニバースから外れた保有銘柄は bundle には後から合流するのに
// **日足の取り込み対象には入らない**ので、DB のバーがその日で凍結する。3 営業日
// (maxDailyCandleAgeTradingDays)を超えると bundle が fail-close して
// `daily candles stale; skipping day-horizon eval` を吐き、day-horizon の評価が
// 止まる。CSV(~/.stockbot/data/*_daily.csv)は新鮮なままなので、**読みに行って
// いないだけ**という気づきにくい形で出る。
//
// 実際に起きた形: 別トラックだけが持つ銘柄がある日に
// ユニバースから外れ、DB の最新バーがその前日で止まっていた(CSV は新鮮)。
//
// 🛑 建玉は **トラックごとに別 DB**(research / live / harvest)なのに日足の repo は
// 3 本で共有している。だから 1 つの repo だけ見ると harvest だけが持つ銘柄を取り
// こぼす。可変長の repos を取るのはそのため。
func TestCandleCoverageSymbolsUnionsHeldAcrossTracks(t *testing.T) {
	research := repository.NewInMemoryPositionRepo()
	harvest := repository.NewInMemoryPositionRepo()
	openPos(t, research, "8354") // ユニバース外・research が保有
	openPos(t, harvest, "3197")  // ユニバース外・harvest だけが保有
	openPos(t, harvest, "7203")  // ユニバース内(重複させない)

	got, err := candleCoverageSymbols(context.Background(), []string{"7203", "6758"}, research, harvest)
	if err != nil {
		t.Fatalf("candleCoverageSymbols: %v", err)
	}
	if strings.Join(got, ",") != "7203,6758,3197,8354" {
		t.Fatalf("取り込み対象 = %v, want [7203 6758 3197 8354]"+
			"(ユニバースの順序を保ち、合流ぶんは銘柄コード昇順で末尾)", got)
	}
}

// nil の repo を飛ばす。live / harvest は env 未設定なら nil で、起動の途中では
// まだ開いていない(seed は openStore の直後に走る)。
func TestCandleCoverageSymbolsSkipsNilRepos(t *testing.T) {
	research := repository.NewInMemoryPositionRepo()
	openPos(t, research, "8354")

	got, err := candleCoverageSymbols(context.Background(), []string{"7203"}, nil, research, nil)
	if err != nil {
		t.Fatalf("candleCoverageSymbols: %v", err)
	}
	if strings.Join(got, ",") != "7203,8354" {
		t.Fatalf("取り込み対象 = %v, want [7203 8354]", got)
	}
}

// 建玉が全てユニバース内なら合流ゼロ(= 現行と完全に同一の集合)。
func TestCandleCoverageSymbolsNoOpWhenAllHeldAreInUniverse(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	openPos(t, repo, "7203")

	got, err := candleCoverageSymbols(context.Background(), []string{"7203", "6758"}, repo)
	if err != nil {
		t.Fatalf("candleCoverageSymbols: %v", err)
	}
	if strings.Join(got, ",") != "7203,6758" {
		t.Fatalf("取り込み対象 = %v, want [7203 6758]", got)
	}
}

// 建玉一覧が引けないときは error を返す。**黙ってユニバースだけに縮退しない** —
// それをやると「取り込み対象が減ったこと」が誰にも見えないまま、いま直している
// バグと同じ状態(バーが凍結)に戻る。呼び出し側が warn してから縮退を選ぶ。
func TestCandleCoverageSymbolsFailsLoudWhenLedgerUnreadable(t *testing.T) {
	if _, err := candleCoverageSymbols(context.Background(), []string{"7203"}, errRepo{}); err == nil {
		t.Fatal("建玉一覧が引けないのに error にならなかった(黙って縮退すると凍結バグが再来する)")
	}
}
