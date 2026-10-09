package main

import (
	"fmt"

	"stockbot/backend/internal/adapter/apiusage"
)

// budgetCap は上限の唯一の出どころ(bot の dashboard が出す数と同じ)。
const budgetCap = apiusage.BrokerDailyRequestCap

// clmidDailyHistory は日足取得の CLMID。窓の中で**既にプールを引いたか**を
// 見分ける唯一の材料なので、記録側(`attachUsageRecorder`)と綴りを揃える。
const clmidDailyHistory = "CLMMfdsGetMarketPriceHistory"

// requestsNeeded は 1 回の実行が送る wire 本数。**login を含める** — 数え落とすと
// 「ちょうど収まる」と言って 1 本超える。
func requestsNeeded(symbols int, updateBench bool) int {
	return historyRequestsNeeded(symbols, updateBench) + 1 // + login
}

// historyRequestsNeeded は日足取得(`CLMMfdsGetMarketPriceHistory`)だけの本数。
// 🛑 二重取得の判定はこちらと比べる。login を混ぜた `requestsNeeded` と比べると、
// 窓のカウンタ(履歴 CLMID しか数えていない)より常に 1 本大きく、**完走した朝の
// 直後でも「まだ引いていない」と誤判定する**。
func historyRequestsNeeded(symbols int, updateBench bool) int {
	if updateBench {
		return symbols + 1 // + bench_topix
	}
	return symbols
}

// alreadyFetchedThisWindow は「この開局窓で既にプールぶんの日足を引いたか」。
//
// 🚨 **上限ガードだけでは足りない**: bot が 8,062 回まで積んだ
// 夕方に 2 回目を打っても 8,062 + 1,550 = 9,612 で**上限の内側**を素通りする。
// それでも残枠を丸ごと食い潰すし、同じ日足を 2 回引くのは純粋な無駄。ここが止めるのは
// **同じ開局窓(05:30 起点)での再実行**だけ — 深夜に完走した後の 07:00 の朝ジョブが同じ銘柄を引き直す
// 二重取得は窓を跨いでいるので対象外で、
// あちらは暦日で見る `daily_updated_today`(stockbot-routine.sh)が受け持つ。
//
// 🛑 判定を `-symbols` 指定の実行に広げない。穴の空いた 1 銘柄を直す修復実行まで
// 止まると、日足の穴を手で埋める経路が消える。
func alreadyFetchedThisWindow(historyCalls int64, historyNeed int, wholePool bool) bool {
	return wholePool && historyCalls >= int64(historyNeed)
}

// duplicateFetchError は二重取得を止めるときの文面。**次の一手**を書く
// (数字だけ出されても人間はソースを読んで逃げ道を探すだけになる)。
func duplicateFetchError(historyCalls int64) error {
	return fmt.Errorf(
		"この開局窓で日足を既に %d 回引いています(朝の取得が完走した後の 2 回目)。"+
			"同じ日足をもう一度引くのは純粋な無駄で、立花の当日枠を 1,500 回ぶん食います。"+
			"CSV の日付だけ見たいなら `-check`(API を叩きません)。"+
			"朝が途中で落ちた等で本当に引き直すなら `-ignore-budget`",
		historyCalls)
}

// checkRequestBudget は「この実行を通すと立花の上限を超えるか」を判定する。
//
// 🛑 これは**予算ガードであって安全ゲートではない**。カウンタのディレクトリが
// 読めない場合 `Snapshot` は 0 を返し、ここは素通しする — fail-close にすると
// 無関係な fs の不調で**日足の更新が止まり**、翌朝の universe 生成ごと落ちる
// (bot は universe を読めないと起動拒否する)。日足が古いほうが復旧コストが
// 高いので、意図的に fail-open にしてある。
func checkRequestBudget(used int64, need, capTotal int) error {
	remain := int64(capTotal) - used
	if int64(need) <= remain {
		return nil
	}
	return fmt.Errorf(
		"立花の当日枠が足りません: 使用済み %d + 今回 %d > 上限 %d(残枠 %d)。"+
			"CSV の日付だけ見たいなら `-check`(API を叩きません)。"+
			"承知の上で打つなら `-ignore-budget`",
		used, need, capTotal, remain)
}

// budgetInputs は 1 回の実行についてガードが見る全部。bool を裸で 3 つ並べると
// 呼び出し側が読めなくなるので構造体にする。
type budgetInputs struct {
	u apiusage.Usage
	// symbols は今回引く銘柄数、bench は bench_topix も更新するか。
	symbols int
	bench   bool
	// wholePool は `-symbols` 未指定(= -out のプール全部)か。修復実行を
	// 二重取得ガードの対象から外すのに要る。
	wholePool bool
	// ignore は `-ignore-budget`。**両方のガードを同時に外す** — 片方だけ外れると
	// 「どっちで落ちたのか」を人間が追う羽目になる。
	ignore bool
}

// enforceRequestBudget は打つ前の検問。**backup と login より前**に呼ぶこと
// (拒否するなら tar.gz も login も無駄)。
func enforceRequestBudget(in budgetInputs) error {
	if in.ignore {
		return nil
	}
	history := in.u.ByCLMID[clmidDailyHistory]
	if alreadyFetchedThisWindow(history, historyRequestsNeeded(in.symbols, in.bench), in.wholePool) {
		return duplicateFetchError(history)
	}
	return checkRequestBudget(in.u.Total, requestsNeeded(in.symbols, in.bench), budgetCap)
}
