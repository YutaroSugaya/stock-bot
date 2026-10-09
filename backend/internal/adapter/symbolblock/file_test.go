package symbolblock

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

var _ port.SymbolBlockStore = (*FileStore)(nil)

func newStore(t *testing.T, now time.Time) (*FileStore, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runtime", "live_symbol_blocks.json")
	return NewFileStore(p, clock.Fixed(now)), p
}

var t0 = time.Date(2026, 10, 2, 8, 30, 0, 0, clock.JST)

func TestFileStore_MissingFileMeansNoBlocks(t *testing.T) {
	s, _ := newStore(t, t0)
	got, err := s.List(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("ファイルが無ければ停止は 0 件: got %v err %v", got, err)
	}
}

func TestFileStore_BlockSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	s, p := newStore(t, t0)
	if err := s.Block(ctx, "6594", "会計不正"); err != nil {
		t.Fatalf("block: %v", err)
	}
	// 再起動 = 新しいインスタンスで同じファイルを読む。
	got, err := NewFileStore(p, clock.Fixed(t0.Add(24*time.Hour))).List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Symbol != "6594" || got[0].Note != "会計不正" || !got[0].BlockedAt.Equal(t0) {
		t.Fatalf("再起動後に停止が残っていない: %+v", got)
	}
}

func TestFileStore_FileShape(t *testing.T) {
	s, p := newStore(t, t0)
	if err := s.Block(context.Background(), "6594", "n"); err != nil {
		t.Fatalf("block: %v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc struct {
		Version int `json:"version"`
		Blocks  map[string]struct {
			BlockedAt string `json:"blocked_at"`
			Note      string `json:"note"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("json: %v", err)
	}
	if doc.Version != 1 || doc.Blocks["6594"].BlockedAt != t0.Format(time.RFC3339) || doc.Blocks["6594"].Note != "n" {
		t.Fatalf("形が違う: %s", raw)
	}
}

// 原子的に書く: tmp を残さず、書き終えたファイルだけがディレクトリに在る。
func TestFileStore_WritesAtomicallyWithoutLeftovers(t *testing.T) {
	ctx := context.Background()
	s, p := newStore(t, t0)
	for _, sym := range []string{"6594", "7203", "9984"} {
		if err := s.Block(ctx, sym, ""); err != nil {
			t.Fatalf("block: %v", err)
		}
	}
	if err := s.Release(ctx, "7203"); err != nil {
		t.Fatalf("release: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range entries {
		if e.Name() != "live_symbol_blocks.json" && e.Name() != "live_symbol_blocks.log" {
			t.Fatalf("書きかけのファイルが残っている: %s", e.Name())
		}
	}
	got, _ := s.List(ctx)
	if len(got) != 2 || got[0].Symbol != "6594" || got[1].Symbol != "9984" {
		t.Fatalf("一覧(銘柄順): %+v", got)
	}
}

func TestFileStore_CorruptFileFailsClosedAndIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	s, p := newStore(t, t0)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"broken_json":     `{"version":1,"blocks":{`,
		"unknown_version": `{"version":2,"blocks":{}}`,
		"bad_time":        `{"version":1,"blocks":{"6594":{"blocked_at":"yesterday"}}}`,
	} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.List(ctx); err == nil {
			t.Errorf("%s: 壊れたファイルを読めたことにした", name)
		}
		if err := s.Block(ctx, "7203", ""); err == nil {
			t.Errorf("%s: 壊れたファイルの上に書いた(人間の停止を消しうる)", name)
		}
		if raw, _ := os.ReadFile(p); string(raw) != body {
			t.Errorf("%s: 壊れたファイルが書き換わった: %s", name, raw)
		}
	}
}

// 人間がファイルを直接編集しても次の判定から効く。
func TestFileStore_ReadsHumanEditsOnNextList(t *testing.T) {
	ctx := context.Background()
	s, p := newStore(t, t0)
	if err := s.Block(ctx, "6594", ""); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"blocks":{"7203":{"blocked_at":"2026-10-02T09:00:00+09:00","note":"hand"}}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(ctx)
	if err != nil || len(got) != 1 || got[0].Symbol != "7203" {
		t.Fatalf("手で書いた内容が効かない: %+v err %v", got, err)
	}
}

func TestFileStore_BlockTwiceKeepsFirstTime(t *testing.T) {
	ctx := context.Background()
	s, p := newStore(t, t0)
	_ = s.Block(ctx, "6594", "a")
	if err := NewFileStore(p, clock.Fixed(t0.Add(time.Hour))).Block(ctx, "6594", "b"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(ctx)
	if len(got) != 1 || !got[0].BlockedAt.Equal(t0) || got[0].Note != "b" {
		t.Fatalf("二度目の停止: %+v", got)
	}
}

func TestFileStore_ReleaseUnknownSymbol(t *testing.T) {
	s, _ := newStore(t, t0)
	if err := s.Release(context.Background(), "6594"); !errors.Is(err, port.ErrSymbolNotBlocked) {
		t.Fatalf("止まっていない銘柄の解除: %v", err)
	}
}

// 停止・解除のたびに JSONL を追記する(止めた判断の答え合わせに使う)。
func TestFileStore_AppendsOperationLog(t *testing.T) {
	ctx := context.Background()
	s, p := newStore(t, t0)
	_ = s.Block(ctx, "6594", "会計不正")
	_ = s.Release(ctx, "6594")
	f, err := os.Open(filepath.Join(filepath.Dir(p), "live_symbol_blocks.log"))
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	defer f.Close()
	var ops []LogEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e LogEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("jsonl: %v", err)
		}
		ops = append(ops, e)
	}
	if len(ops) != 2 || ops[0].Action != "block" || ops[0].Symbol != "6594" || ops[0].Note != "会計不正" ||
		ops[0].At != t0.Format(time.RFC3339) || ops[1].Action != "release" {
		t.Fatalf("操作の記録: %+v", ops)
	}
}
