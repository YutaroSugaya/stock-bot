package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dashboardHTML(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "app", "handler", "web", "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	return string(b)
}

// ⚰️ **harvest トラックは廃止済み**。画面からは**到達できない**ことを縛る。
// タブだけ消して分岐や文言が残ると、ブラウザに保存された track='harvest' から
// 空の台帳画面に落ちる(localStorage は再読込でも残る)。
// サーバ側の harvest コードも持たない。
func TestDashboardHasNoHarvestTrack(t *testing.T) {
	html := dashboardHTML(t)
	for _, banned := range []string{
		"setTrack('harvest')", "tabHarvest", "isHarvest", "HARVEST",
		"harvest_open_positions", "harvest_oldest_position_days", "harvest_watched_symbols",
		"harvest_max_hold_override_days", "STOCKBOT_HARVEST_BOT_CONFIG",
	} {
		if strings.Contains(html, banned) {
			t.Errorf("ダッシュボードに harvest の名残 %q がある — 廃止したトラックへ画面から到達できる", banned)
		}
	}
	// 保存済みの track が paper / live 以外(= 旧 harvest)なら paper に戻す。
	if !strings.Contains(html, "TRACK !== 'paper' && TRACK !== 'live'") {
		t.Error("localStorage の旧 track='harvest' を paper に戻していない")
	}
	// api() は live のエンドポイントへ分岐し続けること。
	if !strings.Contains(html, "'/api/' + TRACK + path") {
		t.Error("api() が track 別のパスを組んでいない — live のエンドポイントに届かない")
	}
}

// 🛑 ratchet の床は**画面にも効く**。旧規則(harvest)と新規則を取り違えないよう、
// 版スタンプ(`ratchet_floor_at_arm`)で分岐すること。
func TestDashboardAppliesTheRatchetFloorPerPositionStamp(t *testing.T) {
	html := dashboardHTML(t)
	if !strings.Contains(html, "p.ratchet_floor_at_arm ? Math.max(armJPY, peak - giveJPY)") {
		t.Error("giveback 線が素の peak − giveback のまま — engine(exit.go)の決済線と静かにずれる")
	}
	if !strings.Contains(html, ": (peak - giveJPY)") {
		t.Error("旧建玉に床を当てている — harvest の測定対象が画面上で入れ替わる")
	}
}

// 監視銘柄の予算は **効いているかどうかを画面から読めなければならない**。
// これは研究モードの標本を切る censoring なので、掛かっていることが黙って画面から
// 消えるのは harvest の打ち切りと同じ質の事故になる。値が JSON にあっても UI が
// 消費していなければ画面には出ない。
func TestDashboardConsumesSymbolBudgetKeys(t *testing.T) {
	html := dashboardHTML(t)
	for _, key := range []string{
		"account_open_symbols",  // いま何銘柄持っているか
		"account_max_symbols",   // 全体の枠
		"per_entry_max_symbols", // 入口ごとの枠
	} {
		if !strings.Contains(html, key) {
			t.Errorf("ダッシュボードが %q を消費していない — 枠が効いているか画面から読めない", key)
		}
	}
}
