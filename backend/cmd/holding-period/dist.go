package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
)

// 保有期間の分布(戦略 × 決済理由)。**この検定の第一の目的** — 現行の ATR 幾何
// (SL ≈ 建値の 9%)での保有期間分布を測る。固定率の旧出口(SL ≈ 2%)の数字は
// バリアまでの距離が約 4.5 倍違うので転用できない。
//
// 単位は**暦日と営業日の両方**を出す。暦日だけだと週末ぶん水増しされ、営業日で
// 切られている MaxHold にぶつかったかを読めない。

type bucket struct {
	Strategy    string  `json:"strategy"`
	CloseReason string  `json:"close_reason"`
	N           int     `json:"n"`
	MedianDays  float64 `json:"median_days"`
	P90Days     float64 `json:"p90_days"`
	MaxDays     float64 `json:"max_days"`
	// 🛑 **営業日**の分布。MaxHold は営業日で切られているので、暦日だけでは
	// 「期限にぶつかったか」を読めない(週末を跨ぐと暦日は 2 日ぶん水増しされ、
	// 7営業日の atr_breakout_v2 が「9日持った」ように見える)。
	MedianBusinessDays float64 `json:"median_business_days"`
	P90BusinessDays    float64 `json:"p90_business_days"`
	MaxBusinessDays    float64 `json:"max_business_days"`
	NetJPY             float64 `json:"net_jpy"`
	// 🛑 **決済時の peak / trough の分布**(採点時に必読と宣言したもの)。
	// 単位は**円/株**(MFE / MAE)。トレールの床は `max(arm, peak − giveback)` なので、
	// 「armed に届いたのか」「届いてからどれだけ返したのか」はこの分布でしか読めない。
	MedianPeakJPY   float64   `json:"median_peak_jpy_per_share"`
	P90PeakJPY      float64   `json:"p90_peak_jpy_per_share"`
	MedianTroughJPY float64   `json:"median_trough_jpy_per_share"`
	RatchetN        int       `json:"ratchet_n"`
	ArmedN          int       `json:"armed_n"`
	samples         []float64 // 暦日(小数)
	bizSamples      []float64 // 営業日(整数)
	peaks           []float64 // 円/株
	troughs         []float64 // 円/株
}

type distReport struct {
	Buckets    []bucket `json:"buckets"`
	N          int      `json:"n"`
	FirstClose string   `json:"first_close"`
	LastClose  string   `json:"last_close"`
	Warnings   []string `json:"warnings,omitempty"`
}

// cycle2Start はサイクル境界。跨いだ標本は出口幾何が別物(円建て → ATR)なので、
// 保有期間を同じ表に並べると**別々の分布を混ぜた 1 本の山**になる。
//
// **`STOCKBOT_CYCLE_SINCE` が唯一の定義**(`stockbot-routine.sh` の自動集計と同じ変数)。
// Go 側に日付を焼くと、次のサイクルが始まった日に**警告が黙って出なくなる**。
var cycle2Start = cycleBoundary()

// tradingHours は営業日換算に使う休場カレンダー。**分布の単位を決めるだけ**で
// 発注には一切関わらない。読めなければ週末だけを飛ばす(祝日を知らないぶん
// 営業日が多めに出るが、暦日しか出せないよりは MaxHold に近い)。
var tradingHours = loadTradingHours()

func loadTradingHours() session.TradingHours {
	for _, p := range []string{"../configs/hard_limits.yaml", "configs/hard_limits.yaml"} {
		hl, err := config.LoadHardLimits(p)
		if err != nil {
			continue
		}
		if th, err := hl.SessionHours.TradingHours(); err == nil {
			return th
		}
	}
	fmt.Fprintln(os.Stderr, "holding-period: ⚠ hard_limits.yaml を読めないので営業日換算は週末だけを飛ばす(祝日ぶん多めに出る)")
	return session.TradingHours{TZ: jst}
}

func cycleBoundary() time.Time {
	if v := os.Getenv("STOCKBOT_CYCLE_SINCE"); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, jst); err == nil {
			return t
		}
	}
	return time.Date(2026, 8, 10, 0, 0, 0, 0, jst)
}

func distribution(positions []port.ClosedPositionSnapshot) distReport {
	rep := distReport{Buckets: []bucket{}}
	key := map[string]*bucket{}
	var first, last time.Time
	for _, p := range positions {
		if p.OpenedAt.IsZero() || p.ClosedAt.Before(p.OpenedAt) {
			continue // 材料にならない行は数えない(0 日として混ぜない)
		}
		rep.N++
		if first.IsZero() || p.ClosedAt.Before(first) {
			first = p.ClosedAt
		}
		if p.ClosedAt.After(last) {
			last = p.ClosedAt
		}
		k := p.Strategy + "|" + p.CloseReason
		b, ok := key[k]
		if !ok {
			b = &bucket{Strategy: p.Strategy, CloseReason: p.CloseReason}
			key[k] = b
		}
		b.N++
		b.NetJPY += p.NetJPY
		b.samples = append(b.samples, p.ClosedAt.Sub(p.OpenedAt).Hours()/24)
		// 🛑 **営業日でも数える**。MaxHold は営業日で切られているので、暦日の
		// 分布だけでは「期限にぶつかったか」を読めない(週末を跨ぐ建玉は暦日で
		// 2 日ぶん水増しされ、7営業日の atr_v2 が「9日持った」ように見える)。
		b.bizSamples = append(b.bizSamples, float64(tradingHours.BusinessDaysBetween(p.OpenedAt, p.ClosedAt)))
		b.peaks = append(b.peaks, p.PeakUnrealizedJPY)
		b.troughs = append(b.troughs, p.TroughUnrealizedJPY)
		if p.RatchetArmJPY > 0 {
			b.RatchetN++
			// 🛑 armed の判定は **peak >= arm**。決済理由(`ratchet_takeprofit` /
			// `ratchet_giveback_loss`)で数えると、armed に届いたのに SL / max_hold で
			// 出た建玉が落ちる。
			if p.PeakUnrealizedJPY >= p.RatchetArmJPY {
				b.ArmedN++
			}
		}
	}
	for _, b := range key {
		sort.Float64s(b.samples)
		sort.Float64s(b.bizSamples)
		b.MedianDays = quantile(b.samples, 0.5)
		b.P90Days = quantile(b.samples, 0.9)
		b.MedianBusinessDays = quantile(b.bizSamples, 0.5)
		b.P90BusinessDays = quantile(b.bizSamples, 0.9)
		if n := len(b.bizSamples); n > 0 {
			b.MaxBusinessDays = b.bizSamples[n-1]
		}
		sort.Float64s(b.peaks)
		sort.Float64s(b.troughs)
		b.MedianPeakJPY = quantile(b.peaks, 0.5)
		b.P90PeakJPY = quantile(b.peaks, 0.9)
		b.MedianTroughJPY = quantile(b.troughs, 0.5)
		if n := len(b.samples); n > 0 {
			b.MaxDays = b.samples[n-1]
		}
		rep.Buckets = append(rep.Buckets, *b)
	}
	sort.Slice(rep.Buckets, func(i, j int) bool {
		if rep.Buckets[i].Strategy != rep.Buckets[j].Strategy {
			return rep.Buckets[i].Strategy < rep.Buckets[j].Strategy
		}
		return rep.Buckets[i].CloseReason < rep.Buckets[j].CloseReason
	})
	if !first.IsZero() {
		rep.FirstClose = first.In(jst).Format("2006-01-02")
		rep.LastClose = last.In(jst).Format("2006-01-02")
		if first.Before(cycle2Start) && !last.Before(cycle2Start) {
			rep.Warnings = append(rep.Warnings,
				"対象がサイクル境界(2026-08-10)を跨いでいる。SL の距離が約4.5倍違い到達時間は距離の2乗で伸びるので、保有期間を合算して読まない — -since で切ること")
		}
	}
	return rep
}

// quantile は nearest-rank(`ceil(q*n)-1`)の**実標本**。線形補間しないのは、N が小さい
// 分布で補間すると存在しない保有期間を「観測」として出すことになるから。
// ⚠ `floor(q*(n-1))` は下側に偏る(n=3 の p90 が 2 番目の標本になる)ので使わない。
func quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(n))) - 1
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	return sorted[i]
}
