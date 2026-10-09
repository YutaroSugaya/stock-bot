package journal

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"stockbot/backend/internal/domain/clock"
)

// stage2AttemptSuffix は「その日に段2 を試した」印。**失敗も含めて**記録する。
const stage2AttemptSuffix = ".stage2.attempt"

// PendingStage2Days は outDir の中で「段1 の JSON はあるが段2 の `.md` が無い」日を
// **新しい順**に返す。bot の引け後ジョブが日記を書き足すための一覧。
//
// 🛑 **当日は返さない。** 当日の日足は翌朝の `fetch-daily` 待ちなので、引け直後に組んだ
// 段1 は市況が必ず「取得できず」になる(実測: 段1 は market が空、
// 翌日に測り直した 08-13 は TOPIX も騰落も入っている)。段2 は `O_EXCL` で二度と
// 書き直せないので、その日に書くと**市況の無い総評が永久に固定される**。
// 呼び手は対象日の段1 を**測り直してから**段2 に渡すこと。
//
// なぜ新しい順か: 古い日で LLM が繰り返し落ちても新しい日の総評が書けなくなる
// (head-of-line blocking)のを避けるため。
//
// なぜ lookback があるか(**暦日**で数える): 何日も落ちていた後に何十日ぶんも回すと、
// 引け後の枠を使い切る。**古い日は諦める**(段1 の JSON は残っているので人間が
// `cmd/daily-review -date ... -stage2` で後から書ける)。
func PendingStage2Days(outDir string, asOf time.Time, max, lookbackDays int) []string {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil // 初回起動でディレクトリが無い等。記録の欠如で騒がない。
	}
	today := asOf.In(clock.JST).Format("2006-01-02")
	oldest := asOf.In(clock.JST).AddDate(0, 0, -lookbackDays).Format("2006-01-02")

	done := map[string]bool{}
	var days []string
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".md"):
			done[strings.TrimSuffix(name, ".md")] = true
		case strings.HasSuffix(name, stage2AttemptSuffix):
			// 今日すでに試した日は再試行しない(再起動のたびに LLM を焼かない)。
			d := strings.TrimSuffix(name, stage2AttemptSuffix)
			if attemptedOn(filepath.Join(outDir, name)) == today {
				done[d] = true
			}
		case strings.HasSuffix(name, ".json"):
			d := strings.TrimSuffix(name, ".json")
			if !isISODate(d) {
				continue // notes.json / ゼロ埋めでない日付(辞書順比較が壊れる)
			}
			if d >= today || d < oldest { // YYYY-MM-DD は辞書順 = 時系列順
				continue
			}
			days = append(days, d)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))

	out := make([]string, 0, len(days))
	for _, d := range days {
		if done[d] {
			continue
		}
		if len(out) >= max {
			break
		}
		out = append(out, d)
	}
	return out
}

// MarkStage2Attempt は「その日にこの日付を試した」印を残す。**成功でも失敗でも呼ぶ**
// (失敗こそが再試行ループの元なので)。印は上書きしてよい。
func MarkStage2Attempt(outDir, date string, asOf time.Time) error {
	return os.WriteFile(filepath.Join(outDir, date+stage2AttemptSuffix),
		[]byte(asOf.In(clock.JST).Format("2006-01-02")+"\n"), 0o644)
}

// ClearStage2Attempt は試行印を消す。**停止で中断されただけ**のときに使う
// (本物の失敗は印を残して連打を防ぐ)。命名規則を1か所に閉じるため port を切る。
func ClearStage2Attempt(outDir, date string) error {
	err := os.Remove(filepath.Join(outDir, date+stage2AttemptSuffix))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func attemptedOn(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// isISODate は **ゼロ埋めの YYYY-MM-DD** だけを通す。Go のパーサは `2026-8-4` も
// 受けるが、辞書順で日付を比較しているので桁が揃っていないと境界が壊れる。
func isISODate(s string) bool {
	if len(s) != len("2006-01-02") {
		return false
	}
	_, err := time.ParseInLocation("2006-01-02", s, clock.JST)
	return err == nil
}
