package main

import (
	"testing"
	"time"

	"stockbot/backend/internal/port"
)

func p(strategy, reason string, openDay, closeDay int, net float64) port.ClosedPositionSnapshot {
	return port.ClosedPositionSnapshot{
		Strategy: strategy, CloseReason: reason, NetJPY: net,
		OpenedAt: time.Date(2026, 8, openDay, 9, 0, 0, 0, jst),
		ClosedAt: time.Date(2026, 8, closeDay, 9, 0, 0, 0, jst),
	}
}

func TestDistribution_ByStrategyAndReason(t *testing.T) {
	rep := distribution([]port.ClosedPositionSnapshot{
		p("atr_breakout_v2", "stop_loss", 12, 13, -100),
		p("atr_breakout_v2", "stop_loss", 11, 14, -200),
		p("atr_breakout_v2", "take_profit", 10, 14, 500),
		p("bnf_reversion", "stop_loss", 13, 14, -50),
	})
	if rep.N != 4 || len(rep.Buckets) != 3 {
		t.Fatalf("buckets = %+v", rep.Buckets)
	}
	// 戦略 → 決済理由の順で安定ソート(日ごとに順序が揺れると差分が読めない)。
	if rep.Buckets[0].Strategy != "atr_breakout_v2" || rep.Buckets[0].CloseReason != "stop_loss" {
		t.Fatalf("並び = %+v", rep.Buckets[0])
	}
	sl := rep.Buckets[0]
	if sl.N != 2 || sl.MaxDays != 3 || sl.NetJPY != -300 {
		t.Fatalf("stop_loss = %+v", sl)
	}
}

// 🛑 分位は**線形補間しない**。N が小さい分布で補間すると、存在しない保有期間を
// 「観測」として出すことになる。
func TestQuantile_UsesRealSamples(t *testing.T) {
	s := []float64{1, 2, 10}
	if got := quantile(s, 0.5); got != 2 {
		t.Fatalf("median = %v, want 2", got)
	}
	if got := quantile(s, 0.9); got != 10 {
		t.Fatalf("p90 = %v, want 10(実標本)", got)
	}
	if got := quantile(nil, 0.5); got != 0 {
		t.Fatalf("空 = %v", got)
	}
}

// opened_at が無い / 逆転している行は**数えない**(0 日として混ぜない)。
func TestDistribution_SkipsUnusableRows(t *testing.T) {
	bad := port.ClosedPositionSnapshot{Strategy: "s", CloseReason: "manual",
		ClosedAt: time.Date(2026, 8, 14, 9, 0, 0, 0, jst)} // OpenedAt ゼロ値
	rep := distribution([]port.ClosedPositionSnapshot{bad})
	if rep.N != 0 || len(rep.Buckets) != 0 {
		t.Fatalf("使えない行を数えている: %+v", rep)
	}
}

// サイクル境界を跨いだら警告する(SL の距離が 4.5 倍違い、到達時間は距離の 2 乗で伸びる)。
func TestDistribution_WarnsAcrossCycleBoundary(t *testing.T) {
	rep := distribution([]port.ClosedPositionSnapshot{
		p("s", "stop_loss", 5, 7, -10),   // 境界の前
		p("s", "stop_loss", 12, 13, -10), // 境界の後
	})
	if len(rep.Warnings) == 0 {
		t.Fatal("境界跨ぎの警告が無い")
	}
}
