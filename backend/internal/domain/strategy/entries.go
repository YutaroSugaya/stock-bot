package strategy

import (
	"fmt"
	"strings"

	"stockbot/backend/internal/config"
)

// ScreenersForEntries は DefaultScreeners()(コードのメニュー)を **入口単位**で絞る。
//
// paper で回す入口は config
// (`advisor_v2.entries`)が持つ。トレンド系 4 入口は**停止**であって棄却ではないので、
// メニューから消さずにここで絞る(再開は一覧に 1 行足すだけ)。
//
// 規則:
//   - 空(nil / 0 件)= 絞らない。このキーを持たない config はメニュー全部で回る(従来どおり)。
//   - 名前は**入口**(`EntryArmOf` で畳んだ名前)で書く。兄弟 `_trail` はその入口と一緒に通る —
//     枠を配る単位が入口なのと同じ理由で、ペアの片脚だけを回す構成を作らない。
//   - 🛑 fail-close: メニューに無い名前・兄弟名・重複は error。黙って落とすと、綴りを誤った
//     アームが**永久に標本ゼロ**のまま走る(high_52w が 4 週間そうだった壊れ方と同型)。
//
// 順序は DefaultScreeners のまま(ランキングの tiebreak は名前なので順序は結果に効かないが、
// 画面と snapshot の並びを安定させる)。
func ScreenersForEntries(entries []config.StrategyName) ([]Screener, error) {
	all := DefaultScreeners()
	if len(entries) == 0 {
		return all, nil
	}
	menu := map[config.StrategyName]bool{}
	for _, s := range all {
		menu[EntryArmOf(s.Name())] = true
	}
	want := map[config.StrategyName]bool{}
	for _, e := range entries {
		if strings.HasSuffix(string(e), TrailArmSuffix) {
			return nil, fmt.Errorf("advisor entries: %q は兄弟アーム名 — 入口名 %q で書く(兄弟は入口と一緒に回る)", e, EntryArmOf(e))
		}
		if !menu[e] {
			return nil, fmt.Errorf("advisor entries: %q はメニュー(DefaultScreeners)に無い入口(綴り / 棄却済みテンプレ)", e)
		}
		if want[e] {
			return nil, fmt.Errorf("advisor entries: %q が重複している", e)
		}
		want[e] = true
	}
	out := make([]Screener, 0, len(all))
	for _, s := range all {
		if want[EntryArmOf(s.Name())] {
			out = append(out, s)
		}
	}
	return out, nil
}
