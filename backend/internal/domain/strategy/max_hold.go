package strategy

import (
	"math"

	"stockbot/backend/internal/config"
)

// TrailArmSuffix は兄弟アームの命名規則(`bnf_reversion_trail` と同じ)。
// 属性(向き・MaxHold)は基のアームから引くので、この 1 文字列が対応表になる。
const TrailArmSuffix = "_trail"

// MaxHoldBusinessDays は戦略別の保有上限(**営業日**)。0 = このテーブルは期限を与えない
// (BNF 3戦略は自前の MaxHold を持つ / no_trade と未知の戦略は建てない)。
//
// 規則は **1つ**: **MaxHold = 入口シグナルのルックバック窓 × 0.5(切り上げ)**
// 根拠は「N 日の窓で計算したシグナルが
// 持つ情報は、おおむね N 日かけて減衰する」で、その半減期を保有の上限に採る。
// 窓はすべて入口の既存定数を参照する — 数字を写すと定数を動かしたとき静かにずれる。
//
// 🛑 **測定中に値を動かさない**。短すぎ/長すぎと分かっても、それは
// **採点結果**であって、次の検定の再登録事項。
// 🛑 **係数(0.5)を戦略ごとに変えない**。割った瞬間 1 つの規則が 6 つの自由パラメータに
// なり forking paths が一気に増える(EDGE_METHODOLOGY)。
//
// ⚠ `high_52w_momentum` は配線バグ(供給 250本 < 要求 253本)で
// 長く建玉ゼロだった。修正後も、broker 直読み fallback に落ちた銘柄では依然として
// 評価されない — この行に MaxHold の効果を読み取るときは標本の出所を確認する。
func MaxHoldBusinessDays(n config.StrategyName) int {
	// 🛑 `_trail` 兄弟アームは基のアームと**同じ** MaxHold。ここを変えると
	// ペアで動く変数が 2 つになる。入口へ畳んで引くのは、テーブルに 2 行書くと片方だけ
	// 直す事故が起きるため(direction も同じ作法 — command.ArmDirection)。
	w, ok := entryLookbackWindow(EntryArmOf(n))
	if !ok {
		return 0
	}
	return (w + 1) / 2 // 窓 × 0.5・切り上げ
}

// applyStrategyMaxHold は MaxHoldBusinessDays の営業日を、建玉時刻(in.Now)を起点とした
// **暦の分数**へ直す(closeAlignedMaxHold)。表に無い戦略は cur をそのまま返す
// — BNF 家族は自前の MaxHold(bnfMaxHoldBusinessDays)を持つ。
//
// 🛑 config の `exit.max_hold_minutes` には**入れない**。hard_limits の
// `max_hold_minutes: {min:5, max:360}` は intraday config 向けの sanity レンジで、
// multiday の期限(7〜126 営業日)はそこを必ず超える。BNF が既にそうしているように、
// 戦略が自分で決める出口幾何は Signal 側に置く(config 検証の対象外)。
func applyStrategyMaxHold(in EvalInput, name config.StrategyName, cur int) int {
	days := MaxHoldBusinessDays(name)
	if days <= 0 {
		return cur
	}
	return closeAlignedMaxHold(in, days)
}

// closeAlignedMaxHold は「建玉時刻から days 営業日目の引け前(ForceFlatAt)」までの分数を返す。
// Position.MaxHoldUntil は
// OpenedAt + 分なので、分は**切り上げ**て期限が 14:50 より前に落ちないようにする。
// 取引時間を持たない呼び手(backtest の日足リプレイ)は「days 営業日後の同時刻」に
// 縮退する(session.MaxHoldDeadline)。
func closeAlignedMaxHold(in EvalInput, days int) int {
	deadline, _ := in.Hours.MaxHoldDeadline(in.Now, days)
	return int(math.Ceil(deadline.Sub(in.Now).Minutes()))
}

// 各戦略の「入口シグナルのルックバック窓」(営業日)。窓の値はカタログ(catalog.go)の入口の行が持つ。
// ok=false = 期限を与えない戦略。
func entryLookbackWindow(n config.StrategyName) (int, bool) {
	s, ok := lookup(n)
	if !ok || s.LookbackDays <= 0 {
		return 0, false
	}
	return s.LookbackDays, true
}
