package strategy

// DailyBarsRequired は**発注経路が供給しなければならない日足の本数**。
// 登録戦略の入口が要求する minBars の最大値で、いまは `high_52w_momentum` の
// `evalGuard(…, h52Window+1)` = 253 が上限。
//
// 🚨 これが 1 箇所に無かったせいで `high_52w_momentum` は
// メニュー追加から長く**建玉ゼロ**だった。供給側(app/bundle.go)に裸の `250`
// が 2 箇所あり、要求 253 に届かないので `Evaluate` は毎回
// `insufficient_daily_history` を返す。**エラーも警告も出ない**ので、11 ラウンドすべてで
// 枠(per_strategy_n=2)は取れているのに標本だけがゼロ、という形で 4 週間見えなかった。
//
// 🛑 供給側で数字を直に書かない。戦略を足して窓が伸びたら**ここだけ**を動かし、
// daily_history_test.go が「全戦略を賄える最小の本数」であることを振る舞いから確かめる。
//
// ⚠ **残る限界**: broker 直読みの fallback 経路(`GetKlines`)は立花の上限が
// **250 本**なので、repo(CSV/DB)が空でそちらに落ちた銘柄では 52週高値は依然として
// 評価されない。「直したから全部動く」ではない。
const DailyBarsRequired = h52Window + 1
