package main

import (
	"strings"
	"testing"
)

// 🚨 **DB をサイクルごとに分けた瞬間に開く穴**。
//
// 段2 の backfill は「段1 JSON はあるが `.md` が無い」日を拾い、**その日の段1 を
// 現在の DSN で測り直して JSON を上書き**してから LLM に渡す。上書きが安全だとする
// 根拠は「台帳と CSV しか読まないので測り直しても同じ数字になる」だが、その前提は
// **DSN を別の DB へ切り替えると崩れる** — 前の DB にある日を、
// その決済が 1 行も無い DB で組み直してしまう。空 DB でもエラーにはならない
// ので全ゼロで上書きされ、**`.md` は O_EXCL なので二度と書き直せない**。
//
// 🛑 **判定そのもののテストは `internal/app/journal` にある。**
// 最初の是正ではこの関数を `cmd/stockbot` の中に置いたが、**その関数のエラー文が
// 案内する手動コマンド**(`cmd/daily-review`)には同じ弁が無く、案内どおりに打つと
// 素通りした(同日の検証ラウンドで発覚)。呼び手が 2 つある安全弁は共有の側に置く。
// ここに残すのは「**bot の引け後ジョブが確かにその共有弁を通っている**」という配線だけ。
func TestAfterCloseRebuildGoesThroughTheSharedFailClose(t *testing.T) {
	src := mustReadSource(t, "afterclose_jobs.go")

	if !strings.Contains(src, "journal.RefuseEmptyRebuild(") {
		t.Error("引け後の backfill が共有の fail-close を通っていない — " +
			"サイクル境界で DSN を取り違えると全ゼロの総評が永久に固定される")
	}
	// 弁は **その backfill の WriteJSON より前**に無ければ意味がない(上書き後に
	// 気づいても遅い)。ファイル全体で最初の WriteJSON と比べると、当日ぶんの段1 を
	// 書く別経路(測り直しではないので弁は不要)を誤検出するため、
	// **弁の直後に来る WriteJSON** で見る。
	guard := strings.Index(src, "journal.RefuseEmptyRebuild(")
	if guard < 0 {
		t.Fatal("fail-close の呼び出しが無い")
	}
	if !strings.Contains(src[guard:], "journal.WriteJSON(") {
		t.Error("fail-close の後に WriteJSON が無い — 弁が守る対象が消えている")
	}
	// 弁より前に、同じ backfill 関数の中で書いていないこと。
	fn := src[strings.LastIndex(src[:guard], "\nfunc "):]
	if w := strings.Index(fn, "journal.WriteJSON("); w >= 0 && w < strings.Index(fn, "journal.RefuseEmptyRebuild(") {
		t.Error("同じ関数の中で弁より前に WriteJSON している — 上書きを止められない")
	}
	// ローカルに再定義して共有版から分岐していないこと。
	if strings.Contains(src, "func refuseEmptyRebuild(") {
		t.Error("cmd/stockbot にローカル版が復活している — 2 つの弁が別々に育つと、" +
			"片方だけ直した状態(まさにこの事故)に戻る")
	}
}

// 🛑 過去日の backfill は **その日に見ていたユニバース**で組むこと。
// `today.txt` は毎朝上書きされるので、過去日をそれで組むと市況が別の銘柄集合の
// 数字になり、段2 の O_EXCL で永久に固定される。
func TestAfterCloseUsesTheArchivedUniverseForPastDays(t *testing.T) {
	src := mustReadSource(t, "afterclose_jobs.go")
	if !strings.Contains(src, "journal.ArchiveUniversePath(") &&
		!strings.Contains(src, "journal.UniversePathFor(") {
		t.Error("過去日の市況を today.txt で組んでいる — その日に見ていた銘柄とズレる")
	}
}
