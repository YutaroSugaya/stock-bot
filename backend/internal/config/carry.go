package config

import (
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/session"
)

// CarryCalc は hard_limits から信用建玉のコスト計算器を組み立てる。bot(決済・reconcile・
// 引け前フラット化)と backtest が同じ 1 つを使う(D-12: 同じ往復に別の net を付けない)。
// 料率は %表記(2.5 = 年2.50%)なので小数へ落とす。営業日判定に取引カレンダーをそのまま
// 使うのは、受渡日が venue カレンダーとずれると日数が狂うため。
func (hl *HardLimits) CarryCalc(hours session.TradingHours) position.CarryCalc {
	settle := hl.Margin.SettlementBusinessDays
	if settle <= 0 {
		settle = 2 // 日本株 T+2(未設定でも受渡日を今日扱いにしない)
	}
	return position.CarryCalc{
		Rates: position.MarginRates{
			BuyAnnualRate:  hl.Margin.BuyAnnualRatePct / 100,
			SellAnnualRate: hl.Margin.SellLendingAnnualPct / 100,
		},
		TZ:             hours.TZ,
		IsTradingDay:   hours.IsTradingDay,
		SettlementDays: settle,
	}
}
