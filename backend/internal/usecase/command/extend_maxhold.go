package command

import (
	"context"
	"errors"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

var ErrInvalidExtendMinutes = errors.New("add_minutes out of range for this position's holding mode")

// 1 回あたりの上限(保有区分ごと)は position.MaxExtendMinutes が SSOT。

// 手動 override は emergency を絶対バイパスしない — an operator must never extend
// risk exposure past a hard gate.
var ErrEmergencyActive = errors.New("emergency_stop active: resume before extending a position")

// ExtendMaxHold pushes out only the bot's TIME-based exit; the broker-side TP/SL
// OCO is untouched, so the downside stop is unaffected. It delegates to the repo
// CAS (WHERE status='OPEN'), and a non-open/unknown id yields (nil, nil) so the
// handler maps it to 404.
type ExtendMaxHold struct {
	repo      port.PositionRepository
	emergency EmergencyController
}

// NewExtendMaxHold: a nil emergency DISABLES the override guard — tests only,
// never production wiring.
func NewExtendMaxHold(r port.PositionRepository, em EmergencyController) *ExtendMaxHold {
	return &ExtendMaxHold{repo: r, emergency: em}
}

func (c *ExtendMaxHold) Execute(ctx context.Context, id int64, addMinutes int) (*port.MaxHoldExtended, error) {
	if addMinutes < 1 {
		return nil, ErrInvalidExtendMinutes
	}
	if c.emergency != nil && c.emergency.Active() {
		return nil, ErrEmergencyActive
	}
	// 上限を知るには保有区分が要る。未知 id は「上限が決められない」ではなく
	// **404**(既存契約)なので、ここで (nil, nil) に落とす。
	p, err := c.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	if addMinutes > position.MaxExtendMinutes(p.HoldingMode) {
		return nil, ErrInvalidExtendMinutes
	}
	res, err := c.repo.ExtendMaxHold(ctx, id, addMinutes)
	if err != nil || res == nil {
		return res, err
	}
	// 🚨 **伸ばしたのは bot 側の時間切れ出口だけ**。broker 側の守りの期日は動かない。
	// 多日保有ではその期日が有限(立花は最大 10 営業日)なので、応答に書かないと
	// 「30 日持てるようにした = その間 SL がある」と読まれる。
	if p.HoldingMode == order.HoldingMultiday {
		res.Warning = "broker 側の守り(逆指値)の期日は**動いていない**。多日保有の守りは " +
			"寄り前の ReplaceProtectiveOrder が取消 → 再発注で延ばす(訂正 API は退役)。" +
			"ログの「守りの置き直し」/「…に失敗」を必ず確認すること"
	}
	return res, nil
}
