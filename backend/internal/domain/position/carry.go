package position

import (
	"time"

	"stockbot/backend/internal/domain/order"
)

// 信用建玉の資金コスト(年率・小数)。既定値は置かない — 捏造すると台帳の net が嘘になる(未設定なら carry 0)。
type MarginRates struct {
	BuyAnnualRate  float64 // 買方金利(一般信用・買い)
	SellAnnualRate float64 // 貸株料(売り)
}

// 信用建玉のオーバーナイト金利。立花証券 e支店の規定(約定金額 × 年率、受渡日の両端入れ、日計りは 1 日分)。
// 受渡日は約定日 + SettlementDays 営業日(日本株は T+2)。両受渡日が同じ営業日シフトを受けるので日数は
// 「両受渡日間の暦日数 + 1」で数える — 土日は両受渡日の内側に入ったときだけ課金される(商慣行と一致)。
type CarryCalc struct {
	Rates          MarginRates
	TZ             *time.Location
	IsTradingDay   func(time.Time) bool // venue カレンダー(週末 + 祝日)
	SettlementDays int                  // 受渡までの営業日数(日本株 = 2)
}

// 営業日探索の上限。休場カレンダーが期限切れ(全日 false)でも無限ループさせない。打ち切っても課金はする。
const maxSettlementScan = 14

// 台帳規約 net = gross − fee + carry に合わせ **負の額**で返す。現物と料率未設定は 0。
func (c CarryCalc) JPY(p Position, closedAt time.Time) float64 {
	if p.ExecKind == order.ExecCash || p.ExecKind == "" {
		return 0 // 現物に金利は無い
	}
	rate := c.Rates.BuyAnnualRate
	if p.Side == order.SideSell {
		rate = c.Rates.SellAnnualRate
	}
	if rate <= 0 {
		return 0 // 料率が入っていない構成: 捏造しない
	}
	principal := p.EntryPrice * float64(p.Quantity)
	if principal <= 0 {
		return 0
	}
	return -principal * rate * float64(c.days(p.OpenedAt, closedAt)) / 365
}

// days counts 受渡日ベースの両端入れ日数(最低 1 日)。
func (c CarryCalc) days(openedAt, closedAt time.Time) int {
	open := c.settlementDate(openedAt)
	close := c.settlementDate(closedAt)
	n := int(close.Sub(open).Hours()/24) + 1 // 両端入れ
	if n < 1 {
		n = 1 // 同日決済 / 異常な時刻順序でも最低 1 日分
	}
	return n
}

// 受渡日 = 取引日の SettlementDays 営業日後(日付は深夜0時に切って日数差を整数にする)。
func (c CarryCalc) settlementDate(t time.Time) time.Time {
	tz := c.TZ
	if tz == nil {
		tz = time.UTC
	}
	local := t.In(tz)
	d := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, tz)
	remaining := c.SettlementDays
	for scanned := 0; remaining > 0 && scanned < maxSettlementScan; scanned++ {
		d = d.AddDate(0, 0, 1)
		if c.IsTradingDay == nil || c.IsTradingDay(d) {
			remaining--
		}
	}
	return d
}
