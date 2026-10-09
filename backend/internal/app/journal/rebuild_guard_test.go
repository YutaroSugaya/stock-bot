package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePacket(t *testing.T, dir, date string, trades, open int) {
	t.Helper()
	var p Packet
	p.Ledger.Date = date
	p.Ledger.Trades.N = trades
	p.Ledger.Open.N = open
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, date+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func packetWith(date string, trades, open int) Packet {
	var p Packet
	p.Ledger.Date = date
	p.Ledger.Trades.N = trades
	p.Ledger.Open.N = open
	return p
}

// 🚨 **対策が対策自身のエラー文に迂回されていた**。
//
// 段2 は「段1 JSON はあるが .md が無い」日を後から拾い、**現在の DSN で測り直して
// JSON を上書き**してから LLM に渡す。DSN を別の DB へ切り替えた後に
// 前の DB にある日を backfill すると、決済が 1 行も無い DB で組み直して全ゼロで上書きし、
// LLM が「取引ゼロの日」の総評を書く。`.md` は O_EXCL なので二度と書き直せない。
//
// 最初の是正は `cmd/stockbot` の中だけに fail-close を置いた。ところが **その関数の
// エラー文が案内する手動コマンド**(`cmd/daily-review`)には同じ弁が無く、
// 案内どおりに打つと素通りした。呼び手が 2 つある安全弁は共有の側に置く。
func TestRefuseEmptyRebuildBlocksAnOverwriteThatLostTheLedger(t *testing.T) {
	dir := t.TempDir()
	writePacket(t, dir, "2026-08-20", 3, 12)

	err := RefuseEmptyRebuild(dir, "2026-08-20", packetWith("2026-08-20", 0, 0))
	if err == nil {
		t.Fatal("空の測り直しが通った — 総評が全ゼロで永久に固定される")
	}
	// 人間がその場で復旧できる案内であること(どの DSN で打ち直すか)。
	if !strings.Contains(err.Error(), "DB を取り違えている") {
		t.Errorf("原因を名指ししていない: %v", err)
	}
	if !strings.Contains(err.Error(), "-db harvest") {
		t.Errorf("復旧手順が具体的でない(旧サイクルの台帳を指す方法): %v", err)
	}
}

// 初回(既存 JSON 無し)は測り直しではないので通す。
func TestRefuseEmptyRebuildAllowsTheFirstWrite(t *testing.T) {
	if err := RefuseEmptyRebuild(t.TempDir(), "2026-08-20", packetWith("2026-08-20", 0, 0)); err != nil {
		t.Fatalf("初回が落ちた: %v", err)
	}
}

// 🛑 **本当に取引ゼロだった日**を止めない(祝日・全見送りの日は普通にある)。
// 既存も 0 件なら、上書きしても失うものは無い。
func TestRefuseEmptyRebuildAllowsAGenuinelyEmptyDay(t *testing.T) {
	dir := t.TempDir()
	writePacket(t, dir, "2026-08-20", 0, 0)
	if err := RefuseEmptyRebuild(dir, "2026-08-20", packetWith("2026-08-20", 0, 0)); err != nil {
		t.Fatalf("元から空の日で落ちた: %v", err)
	}
}

// 中身のある測り直しは当然通す(門を足したせいで backfill が全部止まる、を防ぐ)。
func TestRefuseEmptyRebuildAllowsANonEmptyRebuild(t *testing.T) {
	dir := t.TempDir()
	writePacket(t, dir, "2026-08-20", 3, 12)
	if err := RefuseEmptyRebuild(dir, "2026-08-20", packetWith("2026-08-20", 3, 12)); err != nil {
		t.Fatalf("同じ中身の測り直しが落ちた: %v", err)
	}
}

// 🚨 過去日は **その日に見ていたユニバース**で組む。`today.txt` は毎朝上書きされるので、
// 過去日をそれで組むと市況(universe_n / 上昇 / 下落 / 中央値)が別の銘柄集合の数字になり、
// 段2 の O_EXCL で永久に固定される。
func TestUniversePathFollowsTheTargetDate(t *testing.T) {
	now := time.Date(2026, 8, 24, 16, 0, 0, 0, time.UTC)

	today := UniversePathFor(now, now)
	if !strings.HasSuffix(today, "today.txt") {
		t.Errorf("当日は today.txt を見ること: %q", today)
	}
	past := UniversePathFor(now.AddDate(0, 0, -4), now)
	if !strings.Contains(past, filepath.Join("archive", "2026-08-20.txt")) {
		t.Errorf("過去日は archive/<date>.txt を見ること: %q — "+
			"today.txt で組むと『その日に見ていた銘柄』とズレた市況が永久に固定される", past)
	}
}
