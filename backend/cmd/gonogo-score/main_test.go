package main

import (
	"path/filepath"
	"testing"
	"time"

	adapter "stockbot/backend/internal/adapter/gonogo"
	"stockbot/backend/internal/adapter/symbolblock"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

func TestReadJudgmentsFromSince(t *testing.T) {
	dir := t.TempDir()
	j := &adapter.Journal{Dir: dir}
	for _, d := range []string{"2026-10-01", "2026-10-02"} {
		r := port.GoNoGoRecord{Date: d, Symbol: "6594", Status: port.GoNoGoStatusOK}
		r.Verdict = port.GoNoGoNoGo
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readJudgments(dir, "2026-10-02")
	if err != nil || len(got) != 1 || got[0].Date != "2026-10-02" {
		t.Fatalf("%+v %v", got, err)
	}
}

// 操作の記録は symbolblock.FileStore が書く形をそのまま読む。
func TestReadBlockOpsReadsTheStoreLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "live_symbol_blocks.json")
	s := symbolblock.NewFileStore(p, clock.Fixed(time.Date(2026, 10, 2, 8, 30, 0, 0, clock.JST)))
	if err := s.Block(t.Context(), "6594", "会計不正"); err != nil {
		t.Fatal(err)
	}
	ops, err := readBlockOps(filepath.Join(filepath.Dir(p), "live_symbol_blocks.log"))
	if err != nil || len(ops) != 1 || ops[0].Symbol != "6594" || ops[0].Action != "block" || ops[0].Note != "会計不正" {
		t.Fatalf("%+v %v", ops, err)
	}
	if ops, err := readBlockOps(filepath.Join(t.TempDir(), "none.log")); err != nil || ops != nil {
		t.Fatal("無ければ空")
	}
}

// 採点は「建つ前の最後の判定」を選ぶので、同じ銘柄の行を 1 本に畳まずに全部読む。
func TestReadJudgmentsKeepsEveryRow(t *testing.T) {
	dir := t.TempDir()
	j := &adapter.Journal{Dir: dir}
	for _, v := range []string{port.GoNoGoGo, port.GoNoGoNoGo} {
		r := port.GoNoGoRecord{Date: "2026-10-05", Symbol: "6594", Status: port.GoNoGoStatusOK}
		r.Verdict = v
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readJudgments(dir, "2026-10-02")
	if err != nil || len(got) != 2 || got[0].Verdict != port.GoNoGoGo || got[1].Verdict != port.GoNoGoNoGo {
		t.Fatalf("%+v %v", got, err)
	}
}
