package broker

import (
	"strings"
	"time"
)

// APIUpdateNotice は login 応答(CLMAuthLoginAck)が毎回返す**予定日の告知** 2 項目。
// 値は YYYYMMDD(応答例 "20250531")。未定は "" または "0"。
// 今回の v4r9 → v4r10 移行も、bot は毎日ログインしながらこの項目を一言も読んでいなかった。
type APIUpdateNotice struct {
	APISpecFunction string // sUpdateInformAPISpecFunction: ｅ支店・ＡＰＩリリース予定日(版の追加・旧版の廃止)
	WebDocument     string // sUpdateInformWebDocument: 交付書面更新予定日(予定日までに標準Webで書面確認)
}

// APIUpdateNotice は直近の login 応答が返した告知(未 login は空)。ログに出すのは cmd 側
// (adapter は slog を知らない)。
func (t *Tachibana) APIUpdateNotice() APIUpdateNotice {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.updateNotice
}

const apiUpdateDateLayout = "20060102"

// APIUpdateDue は v4r10 リファレンス(v10:231【注意２】)の判定式そのもの:
// 「(予定日 ≧ 当日日付) AND (予定日 != 前回受信値(初回は空白として処理))」。
// 🛑 fail-close にしない: 空 / "0" / 解釈不能は false を返すだけで error を出さない
// (告知が出た日に bot が起動しなくなる方が危険)。当日日付は呼び手が clock.Clock から渡す。
func APIUpdateDue(planned, last string, today time.Time) bool {
	planned = strings.TrimSpace(planned)
	if planned == "" || planned == "0" {
		return false
	}
	d, err := time.ParseInLocation(apiUpdateDateLayout, planned, today.Location())
	if err != nil {
		return false
	}
	y, m, dd := today.Date()
	if d.Before(time.Date(y, m, dd, 0, 0, 0, 0, today.Location())) {
		return false
	}
	return planned != strings.TrimSpace(last)
}
