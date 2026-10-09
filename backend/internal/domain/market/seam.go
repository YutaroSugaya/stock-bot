package market

import "sort"

// SeamBreak は取得した日足を保存済みの履歴へ継いでよいかを検査する。継いではいけないとき
// 理由を返す(空 = 継いでよい)。
//
//   - 重なり日の水準が食い違う = 調整規約の異なる 2 ソースを混ぜた兆候(分割未調整の疑い)
//   - 継ぎ目(最後の保存バー)+ 新規バーに分割相当の断裂がある
//
// 🛑 **既存履歴の内部は見ない**。過去の値幅制限いっぱいの実暴落(ローカル日足に 33 件実在)で
// 銘柄の更新が恒久停止し、BNF が最も狙いたい銘柄が静かにユニバースから消えるため。
func SeamBreak(stored, fetched []Candle) string {
	if d := SameDayLevelMismatch(RecentWindow(stored, SeamOverlapDays), fetched); d != "" {
		return "重なり日の水準が不一致(分割未調整の疑い): " + d
	}
	if d := SplitDiscontinuity(SeamWindow(stored, fetched)); d != "" {
		return "継ぎ目/新規バーに断裂(分割未調整の疑い): " + d
	}
	return ""
}

// SeamWindow は [最後の保存バー] + [それより新しい取得バー] を返す。stored は昇順、
// fetched は順不同でよい。
func SeamWindow(stored, fetched []Candle) []Candle {
	fresh := append([]Candle(nil), fetched...)
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].OpenTime.Before(fresh[j].OpenTime) })
	if len(stored) == 0 {
		return fresh
	}
	last := stored[len(stored)-1]
	out := []Candle{last}
	for _, c := range fresh {
		if c.OpenTime.After(last.OpenTime) {
			out = append(out, c)
		}
	}
	return out
}
