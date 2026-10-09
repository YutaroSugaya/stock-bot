package pg

import (
	"stockbot/backend/internal/domain/clock"
	"testing"
	"time"
)

// config_id の時刻プレフィックス(JST)のパース。sentinel(`external`)や手動
// config は形式外なので ok=false — 時刻 fallback の対象にしない。
func TestParseConfigIDTime(t *testing.T) {
	got, ok := parseConfigIDTime("20260730-143130-3436")
	if !ok {
		t.Fatal("advisor config_id はパースできるべき")
	}
	jst := clock.JST
	want := time.Date(2026, 7, 30, 14, 31, 30, 0, jst)
	if !got.Equal(want) {
		t.Fatalf("parsed = %v, want %v", got, want)
	}
	for _, bad := range []string{"external", "no_trade_default_7203", "", "2026-07-30-143130"} {
		if _, ok := parseConfigIDTime(bad); ok {
			t.Fatalf("形式外の config_id %q がパースされた", bad)
		}
	}
}

// input_json(MarketSummary JSON)から advisory.screens[] の戦略別 score を抜く。
// 壊れた JSON は空 map(黙って 0 点にしない — 復元不能側に倒す)。
func TestScreenScores(t *testing.T) {
	j := `{"symbol":"7203","advisory":{"screens":[
		{"symbol":"7203","strategy":"abs_momentum","triggered":true,"score":1.42},
		{"symbol":"7203","strategy":"donchian_breakout","triggered":false,"score":0.98}
	]}}`
	got := screenScores(j)
	if got["abs_momentum"] != 1.42 || got["donchian_breakout"] != 0.98 {
		t.Fatalf("screens が抜けていない: %+v", got)
	}
	if len(screenScores("not json")) != 0 {
		t.Fatal("壊れた JSON は空 map であるべき")
	}
}
