// Package symbolblock は live の銘柄ごとの新規停止をファイルに置く adapter。
//
// 形: {"version":1,"blocks":{"<symbol>":{"blocked_at":"<RFC3339>","note":"<任意>"}}}
//
//   - ファイルが無ければ停止は 0 件。読めない・壊れているときは error を返し、呼び手が
//     live の新規を全部止める(fail-close)。壊れたファイルの上には書かない(人間の停止を消しうる)。
//   - 読むのは毎回(数百バイト)。人間がファイルを直接編集しても次の判定から効く。
//   - 書くのは bot だけで、tmp に書いて fsync してから rename する(原子的)。
//   - 停止・解除のたびに、同じディレクトリの `<名前>.log` へ JSONL を追記する。
package symbolblock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

const fileVersion = 1

type fileDoc struct {
	Version int                  `json:"version"`
	Blocks  map[string]fileBlock `json:"blocks"`
}

type fileBlock struct {
	BlockedAt string `json:"blocked_at"`
	Note      string `json:"note,omitempty"`
}

// LogEntry は操作の記録 1 行(JSONL)。
type LogEntry struct {
	At     string `json:"at"`
	Symbol string `json:"symbol"`
	Action string `json:"action"` // block / release
	Note   string `json:"note,omitempty"`
}

// FileStore は port.SymbolBlockStore のファイル実装。
type FileStore struct {
	path    string
	logPath string
	clock   clock.Clock
	mu      sync.Mutex // 書き込みの直列化(読みは毎回ファイルから)
}

// NewFileStore は path に停止の一覧を置く store を返す。操作の記録は拡張子を .log に替えたパス。
func NewFileStore(path string, c clock.Clock) *FileStore {
	if c == nil {
		c = clock.System()
	}
	return &FileStore{path: path, logPath: strings.TrimSuffix(path, filepath.Ext(path)) + ".log", clock: c}
}

// Path は停止の一覧のパス(起動ログと画面用)。
func (s *FileStore) Path() string { return s.path }

// List は停止中の銘柄を銘柄順に返す。
func (s *FileStore) List(_ context.Context) ([]port.SymbolBlock, error) {
	doc, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]port.SymbolBlock, 0, len(doc.Blocks))
	for sym, b := range doc.Blocks {
		at, _ := time.Parse(time.RFC3339, b.BlockedAt) // read が検査済み
		out = append(out, port.SymbolBlock{Symbol: sym, BlockedAt: at, Note: b.Note})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

// Block はその銘柄の新規を止める。既に止まっていれば止めた時刻を保ち、note だけ差し替える。
func (s *FileStore) Block(_ context.Context, symbol, note string) error {
	if symbol == "" {
		return errors.New("symbol is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	now := s.clock()
	b, ok := doc.Blocks[symbol]
	if !ok {
		b.BlockedAt = now.Format(time.RFC3339)
	}
	b.Note = note
	doc.Blocks[symbol] = b
	if err := s.write(doc); err != nil {
		return err
	}
	return s.appendLog(LogEntry{At: now.Format(time.RFC3339), Symbol: symbol, Action: "block", Note: note})
}

// Release は停止を解く。止まっていなければ port.ErrSymbolNotBlocked。
func (s *FileStore) Release(_ context.Context, symbol string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	b, ok := doc.Blocks[symbol]
	if !ok {
		return fmt.Errorf("%w: %s", port.ErrSymbolNotBlocked, symbol)
	}
	delete(doc.Blocks, symbol)
	if err := s.write(doc); err != nil {
		return err
	}
	return s.appendLog(LogEntry{At: s.clock().Format(time.RFC3339), Symbol: symbol, Action: "release", Note: b.Note})
}

// read はファイルを読んで検査する。無ければ空の一覧。
func (s *FileStore) read() (fileDoc, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return fileDoc{Version: fileVersion, Blocks: map[string]fileBlock{}}, nil
	}
	if err != nil {
		return fileDoc{}, fmt.Errorf("symbol blocks: read %s: %w", s.path, err)
	}
	var doc fileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fileDoc{}, fmt.Errorf("symbol blocks: parse %s: %w", s.path, err)
	}
	if doc.Version != fileVersion {
		return fileDoc{}, fmt.Errorf("symbol blocks: %s の version %d は読めない(%d のみ)", s.path, doc.Version, fileVersion)
	}
	if doc.Blocks == nil {
		doc.Blocks = map[string]fileBlock{}
	}
	for sym, b := range doc.Blocks {
		if sym == "" {
			return fileDoc{}, fmt.Errorf("symbol blocks: %s に空の銘柄がある", s.path)
		}
		if _, err := time.Parse(time.RFC3339, b.BlockedAt); err != nil {
			return fileDoc{}, fmt.Errorf("symbol blocks: %s の %s の blocked_at が読めない: %w", s.path, sym, err)
		}
	}
	return doc, nil
}

// write は tmp に書いて fsync してから rename する。途中で落ちても前の版か次の版のどちらかが残る。
func (s *FileStore) write(doc fileDoc) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("symbol blocks: mkdir: %w", err)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".live_symbol_blocks-*.tmp")
	if err != nil {
		return fmt.Errorf("symbol blocks: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("symbol blocks: write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("symbol blocks: fsync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("symbol blocks: close tmp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		cleanup()
		return fmt.Errorf("symbol blocks: rename: %w", err)
	}
	// rename をディスクへ確定させる(電源断で前の版に戻らないように)。
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *FileStore) appendLog(e LogEntry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("symbol blocks: 停止の状態は書いたが操作の記録を開けない: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("symbol blocks: 停止の状態は書いたが操作の記録を書けない: %w", err)
	}
	return nil
}
