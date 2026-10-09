package market_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"stockbot/backend/internal/domain/market"
)

// ticker らしい変数からの読みだけを拾う(Candle / bar の Volume は対象外)。
var tickerVolumeRead = regexp.MustCompile(`\b(tk|tkr|ticker|quote)\.Volume\b`)

// 🛑 `Ticker.Volume` を読んでよいのは**記録の経路だけ**
// (Aggregator が増分を積む / adapter が wire から詰める)。戦略・risk gate・
// dashboard から読んだ時点で、入口が途中で変わったことになる。
// 当日出来高を使う戦略は別の入口として登録する。
//
// 構造の防壁: 戦略が受け取るのは MarketSummary であって Ticker ではないので、
// **MarketSummary に出来高が載っていない限り戦略からは読めない**。
func TestMarketSummary_HasNoVolume(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf(market.MarketSummary{}),
		reflect.TypeOf(market.CurrentRate{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			if strings.Contains(strings.ToLower(typ.Field(i).Name), "volume") {
				t.Fatalf("%s.%s — 出来高を戦略の入力に載せない(事前登録してから)",
					typ.Name(), typ.Field(i).Name)
			}
		}
	}
}

// 読み手を機械で押さえる。許すのは記録の経路(aggregator)と adapter だけ。
func TestTickerVolume_ReadOnlyByRecordingPath(t *testing.T) {
	root := filepath.Join("..", "..", "..") // backend/
	allowed := map[string]bool{
		filepath.Join("internal", "domain", "market", "aggregator.go"): true,
		filepath.Join("internal", "domain", "market", "tick.go"):       true,
	}
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if allowed[rel] || strings.HasPrefix(rel, filepath.Join("internal", "adapter", "broker")) {
			return nil // wire から詰める側と、増分を積む側
		}
		blob, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, line := range strings.Split(string(blob), "\n") {
			// ticker 変数から直接読んでいる形だけを見る。**日足 Candle.Volume は対象外** —
			// 前日までの出来高は従来から戦略が読んでいる。禁じたいのは
			// **当日の**出来高が判定に混ざること。
			if tickerVolumeRead.MatchString(line) {
				offenders = append(offenders, rel+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("Ticker.Volume を記録以外から読んでいる(入口が途中で変わる):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
