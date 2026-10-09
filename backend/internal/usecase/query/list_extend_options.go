package query

import (
	"context"
	"math"
	"time"

	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
)

// ExtendOption は max_hold 延長の候補 1 本。画面のセレクトはこれを並べ、選ばれた
// AddMinutes をそのまま延長 API(POST …/positions/extend)へ渡す。
//
// Until は**延長後の期限そのもの**(今の期限 + AddMinutes)。domain の MaxHoldUntil は
// OpenedAt + 分なので、分を切り上げたぶん 14:50 に数秒が付く(期限が 14:50 より前に
// 落ちないように — closeAlignedMaxHold と同じ作法)。
type ExtendOption struct {
	Until      time.Time `json:"until"`
	AddMinutes int       `json:"add_minutes"`
	// BusinessDays は今の期限から何営業日先か(0 = 今の期限と同じ日の引け前)。
	BusinessDays int `json:"business_days"`
	// BeyondCalendar = 休場カレンダーの horizon の外(祝日を確かめていない日)。
	// 祝日に落ちた期限は翌営業日の寄りで成行になる(害はないが、表示と実際がずれる)。
	BeyondCalendar bool `json:"beyond_calendar"`
}

type ExtendOptionsView struct {
	PositionID  int64  `json:"position_id"`
	Symbol      string `json:"symbol"`
	HoldingMode string `json:"holding_mode"`
	// nil = 期限の無い建玉。延長を足すと期限を**新しく作る**ことになるので候補は出さない。
	CurrentUntil  *time.Time     `json:"current_until"`
	MaxAddMinutes int            `json:"max_add_minutes"`
	Options       []ExtendOption `json:"options"`
}

// ListExtendOptions は延長先の候補を「各営業日の引け前(ForceFlatAt)」で列挙する。
//
// 🛑 **候補はサーバが出す。** 休場日を知っているのは hard_limits のカレンダーだけで、
// 画面で週末だけ飛ばすと祝日の 14:50 を選べてしまう。1 回あたりの上限も延長の実行と
// 同じ position.MaxExtendMinutes を読む(画面に 30 日を焼かない)。
//
// 伸ばすのは bot 側の時間切れ決済だけ。broker 側の守りの期日は ReplaceProtectiveOrder が
// 寄り前に取消 → 再発注で延ばす(ここでは何もしない)。
type ListExtendOptions struct {
	repo  port.PositionRepository
	hours session.TradingHours
}

func NewListExtendOptions(r port.PositionRepository, th session.TradingHours) *ListExtendOptions {
	return &ListExtendOptions{repo: r, hours: th}
}

// Execute: 未知 id / OPEN でない建玉は (nil, nil) — handler が 404 に写す
// (延長 API の「no open position」と同じ契約)。
func (q *ListExtendOptions) Execute(ctx context.Context, id int64) (*ExtendOptionsView, error) {
	p, err := q.repo.GetByID(ctx, id)
	if err != nil || p == nil || p.Status != position.StatusOpen {
		return nil, err
	}
	maxAdd := position.MaxExtendMinutes(p.HoldingMode)
	v := &ExtendOptionsView{
		PositionID: p.ID, Symbol: p.Symbol, HoldingMode: string(p.HoldingMode),
		MaxAddMinutes: maxAdd, Options: []ExtendOption{},
	}
	cur := p.MaxHoldUntil()
	if cur.IsZero() {
		return v, nil
	}
	v.CurrentUntil = &cur
	limit := cur.Add(time.Duration(maxAdd) * time.Minute)
	for _, d := range q.hours.CloseAlignedDeadlines(cur, limit) {
		add := int(math.Ceil(d.Sub(cur).Minutes()))
		if add < 1 || add > maxAdd {
			continue // 切り上げで上限を 1 分越える最終候補は出さない(延長 API が 400 で弾く)
		}
		until := cur.Add(time.Duration(add) * time.Minute)
		v.Options = append(v.Options, ExtendOption{
			Until:      until,
			AddMinutes: add,
			// 秒の付いた until で数える。d(14:50:00)で数えると cur(14:50:10)から見て
			// 1 日ぶん手前に落ち、翌営業日が「0 営業日先」になる。
			BusinessDays:   q.hours.BusinessDaysBetween(cur, until),
			BeyondCalendar: !q.hours.CalendarCovers(d),
		})
	}
	return v, nil
}
