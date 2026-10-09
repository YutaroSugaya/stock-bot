package main

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/market"
)

// ベンチが壊れると「TOPIX に勝てていない戦略」を勝ちと判定しうる。strict 拒否だけ
// だった頃は 1306 の 1:10 分割で更新が止まり、7週間ベンチが古いまま
// 気づかれなかった(分割前 217 日ぶんが 1円まで一致し、03-30 を境に
// 比率がちょうど 10.0 に変わることを実測済み)。
func benchDay(day string, close float64) market.Candle {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		panic(err)
	}
	return market.Candle{
		OpenTime: t.UTC(), Open: close, High: close, Low: close, Close: close, Volume: 1,
	}
}

func TestMergeBenchmarkChainLinksSplit(t *testing.T) {
	existing := []market.Candle{ // 分割前の水準
		benchDay("2026-06-16", 4200), benchDay("2026-06-17", 4255), benchDay("2026-06-18", 4323),
	}
	fetched := []market.Candle{ // 立花(分割後 = 1/10)
		benchDay("2026-06-18", 432.3), benchDay("2026-06-19", 435.0), benchDay("2026-06-22", 440.0),
	}

	merged, err := mergeBenchmark(existing, fetched)
	if err != nil {
		t.Fatalf("分割を吸収できずに拒否した: %v", err)
	}
	if len(merged) != 5 {
		t.Fatalf("本数 = %d, want 5: %+v", len(merged), merged)
	}
	if got := merged[0].Close; got < 419 || got > 421 {
		t.Errorf("古い側が chain-link されていない: 先頭 close = %.2f, want ≈420", got)
	}
	if got := merged[len(merged)-1].Close; got != 440.0 {
		t.Errorf("新しい側を触ってはいけない: 末尾 close = %.2f, want 440", got)
	}
	if d := market.SplitDiscontinuity(merged); d != "" {
		t.Errorf("chain-link 後に断裂が残っている: %s", d)
	}
}

// 通すと「TOPIX 超過」が静かに別のベンチとの比較に化ける。
func TestMergeBenchmarkRejectsNonSplitLevelMismatch(t *testing.T) {
	existing := []market.Candle{
		benchDay("2026-06-17", 4255), benchDay("2026-06-18", 4323),
	}
	// 1.37倍 — 単純分割比のどれでもない(= 別系列)。
	fetched := []market.Candle{
		benchDay("2026-06-18", 5922), benchDay("2026-06-19", 5950),
	}
	if _, err := mergeBenchmark(existing, fetched); err == nil {
		t.Fatal("分割比でない水準差を受け入れた(ベンチが別系列に化ける)")
	}
}

func TestMergeBenchmarkPassesNormalSession(t *testing.T) {
	existing := []market.Candle{benchDay("2026-06-17", 4255), benchDay("2026-06-18", 4323)}
	fetched := []market.Candle{benchDay("2026-06-18", 4323), benchDay("2026-06-19", 4290)}

	merged, err := mergeBenchmark(existing, fetched)
	if err != nil {
		t.Fatalf("通常の値動きを拒否した: %v", err)
	}
	if len(merged) != 3 || merged[0].Close != 4255 || merged[2].Close != 4290 {
		t.Errorf("素通しになっていない: %+v", merged)
	}
}

// 空を書くと履歴が消える(休場日は取得側が空になる)。
func TestMergeBenchmarkKeepsExistingWhenNothingFetched(t *testing.T) {
	existing := []market.Candle{benchDay("2026-06-18", 4323)}
	merged, err := mergeBenchmark(existing, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(merged) != 1 || merged[0].Close != 4323 {
		t.Errorf("既存が壊れた: %+v", merged)
	}
}
