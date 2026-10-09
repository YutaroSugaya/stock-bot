package strategy

import (
	"testing"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/session"
)

func maxHoldTZ(t *testing.T) *time.Location {
	t.Helper()
	tz, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	return tz
}

// 山の日(2026-08-11)だけを持つ最小の休場カレンダー。
func maxHoldHours(t *testing.T) session.TradingHours {
	t.Helper()
	return session.TradingHours{
		TZ:              maxHoldTZ(t),
		Holidays:        map[string]struct{}{"2026-08-11": {}},
		CalendarThrough: time.Date(2026, 12, 31, 0, 0, 0, 0, maxHoldTZ(t)),
	}
}

// candIn は time.Now() を使うので MaxHold の期限が検証できない。時刻とカレンダーを固定する。
func maxHoldIn(t *testing.T, name config.StrategyName, daily []market.Candle, now time.Time) EvalInput {
	t.Helper()
	in := candIn(name, daily, 1)
	in.Now = now
	in.Hours = maxHoldHours(t)
	return in
}

// MaxHold = 入口の窓 × 0.5(**営業日**)。暦日換算(×1440)ではない。
func TestEnterIfClearsAppliesStrategyMaxHoldInBusinessDays(t *testing.T) {
	tz := maxHoldTZ(t)
	// 2026-08-24(月)09:30 に建てる。donchian_breakout_v2 は 10 営業日 →
	// 09-07(月)09:30。間に週末が 2 回(4日)入るので 14 暦日 = 20,160 分。
	now := time.Date(2026, 8, 24, 9, 30, 0, 0, tz)
	d := dbV2FirstDay()
	sig := DonchianBreakoutV2{}.Evaluate(maxHoldIn(t, config.StrategyDonchianBreakoutV2, d, now))
	if !sig.IsEntry() {
		t.Fatalf("前提: ENTER であること: %q", sig.Reason)
	}
	want := 14 * 24 * 60
	if sig.MaxHoldMinutes != want {
		t.Fatalf("MaxHoldMinutes = %d, want %d(10 営業日 = 14 暦日)", sig.MaxHoldMinutes, want)
	}
	// 暦日換算(10×1440)に落ちていないことを明示的に落とす。
	if sig.MaxHoldMinutes == 10*1440 {
		t.Fatal("暦日換算(10日)になっている — 営業日換算でなければならない")
	}
}

// 休場日を跨ぐと期限は暦日ぶん伸びる(山の日を含む窓で確認)。
func TestEnterIfClearsMaxHoldSkipsMarketHolidays(t *testing.T) {
	tz := maxHoldTZ(t)
	// 2026-08-07(金)建て・atr_breakout_v2 は 7 営業日。
	// 08-10(月) 1、08-11(火・山の日)は数えず、08-12〜14 で 4、08-17〜19 で 7 →
	// 着地は 08-19(水)。= 12 暦日。
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, tz)
	d := atrV2Compressed()
	sig := ATRBreakoutV2{}.Evaluate(maxHoldIn(t, config.StrategyATRBreakoutV2, d, now))
	if !sig.IsEntry() {
		t.Fatalf("前提: ENTER であること: %q", sig.Reason)
	}
	if want := 12 * 24 * 60; sig.MaxHoldMinutes != want {
		t.Fatalf("MaxHoldMinutes = %d, want %d(7 営業日・山の日を跨ぐ)", sig.MaxHoldMinutes, want)
	}
}

// 🛑 出口の他の脚は一切動かさない(TP/SL の ATR 係数を触らない)。
func TestEnterIfClearsMaxHoldLeavesATRGeometryUntouched(t *testing.T) {
	tz := maxHoldTZ(t)
	now := time.Date(2026, 8, 24, 9, 30, 0, 0, tz)
	d := absV2FreshCross()
	withHours := AbsMomentumV2{}.Evaluate(maxHoldIn(t, config.StrategyAbsMomentumV2, d, now))
	if !withHours.IsEntry() {
		t.Fatalf("前提: ENTER: %q", withHours.Reason)
	}
	bare := AbsMomentumV2{}.Evaluate(candIn(config.StrategyAbsMomentumV2, d, 1))
	if withHours.TakeProfitJPY != bare.TakeProfitJPY || withHours.StopLossJPY != bare.StopLossJPY {
		t.Fatalf("MaxHold の導入が TP/SL を動かしている: TP %v→%v SL %v→%v",
			bare.TakeProfitJPY, withHours.TakeProfitJPY, bare.StopLossJPY, withHours.StopLossJPY)
	}
	if withHours.RatchetArmJPY != 0 || withHours.RatchetGivebackJPY != 0 {
		t.Fatal("ratchet を勝手に付けている(v2 は ratchet なし)")
	}
}

// カレンダーが無い(TZ nil = backtest の日足リプレイ)ときも**期限は付く**。
// 週末だけを飛ばす営業日になる — 祝日を知らないぶん実際より手前に落ちるが、
// 黙って「無期限」に縮退させない(それでは期限の検定が backtest で消える)。
func TestEnterIfClearsMaxHoldFallsBackToWeekdaysWithoutCalendar(t *testing.T) {
	d := dbV2FirstDay()
	in := candIn(config.StrategyDonchianBreakoutV2, d, 1)
	in.Now = time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC) // 月曜・Hours は zero 値
	sig := DonchianBreakoutV2{}.Evaluate(in)
	if !sig.IsEntry() {
		t.Fatalf("前提: ENTER: %q", sig.Reason)
	}
	if want := 14 * 24 * 60; sig.MaxHoldMinutes != want {
		t.Fatalf("MaxHoldMinutes = %d, want %d(カレンダー無しでも週末は飛ばす)", sig.MaxHoldMinutes, want)
	}
}

// 多日建玉の時間切れは **N 営業日目の引け前(14:50)**。取引時間
// (Sessions / ForceFlatAt)を持つ呼び手では期限をその時刻に揃える(損益に依らずその時刻で決済)。
// BNF 家族は 10 営業日。
func sessionHours(t *testing.T) session.TradingHours {
	t.Helper()
	h := maxHoldHours(t)
	h.Sessions = []session.Window{{Start: "09:00", End: "11:30"}, {Start: "12:30", End: "15:30"}}
	h.ForceFlatAt = "14:50"
	h.Holidays = map[string]struct{}{"2026-09-21": {}, "2026-09-22": {}, "2026-09-23": {}}
	return h
}

func TestBNFFamilyMaxHoldIsTenBusinessDaysAtForceFlat(t *testing.T) {
	tz := maxHoldTZ(t)
	now := time.Date(2026, 9, 14, 9, 1, 24, 0, tz)
	want := time.Date(2026, 10, 1, 14, 50, 0, 0, tz) // シルバーウィーク(9/21〜23)を跨ぐ
	in := candIn(config.StrategyBNFReversion, bnfPanicSeries(), 1)
	in.Now, in.Hours = now, sessionHours(t)
	for _, sig := range []Signal{BNFReversion{}.Evaluate(in), BNFReversionTrail{}.Evaluate(in)} {
		if !sig.IsEntry() {
			t.Fatalf("前提: ENTER: %q", sig.Reason)
		}
		got := now.Add(time.Duration(sig.MaxHoldMinutes) * time.Minute)
		if got.Before(want) || got.Sub(want) >= time.Minute {
			t.Fatalf("%s: 期限 = %v, want %v(10 営業日目の 14:50)", sig.StrategyName, got, want)
		}
	}
}

func TestTableStrategyMaxHoldAlignsToForceFlat(t *testing.T) {
	tz := maxHoldTZ(t)
	// donchian_breakout_v2 は 10 営業日。9/7 10:20 建て → 9/24 14:50。
	now := time.Date(2026, 9, 7, 10, 20, 24, 0, tz)
	in := maxHoldIn(t, config.StrategyDonchianBreakoutV2, dbV2FirstDay(), now)
	in.Hours = sessionHours(t)
	sig := DonchianBreakoutV2{}.Evaluate(in)
	if !sig.IsEntry() {
		t.Fatalf("前提: ENTER: %q", sig.Reason)
	}
	want := time.Date(2026, 9, 24, 14, 50, 0, 0, tz)
	got := now.Add(time.Duration(sig.MaxHoldMinutes) * time.Minute)
	if got.Before(want) || got.Sub(want) >= time.Minute {
		t.Fatalf("期限 = %v, want %v", got, want)
	}
}
