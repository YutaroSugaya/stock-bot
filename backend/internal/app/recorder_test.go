package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
)

func rbar(t time.Time, iv time.Duration, c float64) market.Candle {
	return market.Candle{
		Symbol: "7203", OpenTime: t, Interval: iv,
		Open: c - 1, High: c + 1, Low: c - 2, Close: c, Volume: 100,
	}
}

func TestCandleRecorder_AppendsOnlyCompletedBarsOnce(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 21, 9, 0, 0, 0, clock.JST)
	now := base.Add(15 * time.Minute)

	r := NewCandleRecorder(dir)
	bars := []market.Candle{
		rbar(base, 5*time.Minute, 100),
		rbar(base.Add(5*time.Minute), 5*time.Minute, 101),
		rbar(base.Add(10*time.Minute), 5*time.Minute, 102), // completes exactly at now
		rbar(base.Add(15*time.Minute), 5*time.Minute, 103), // FORMING — must not be written
	}
	n, err := r.Record("7203", 5*time.Minute, bars, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("wrote %d bars, want 3 (forming bar excluded)", n)
	}

	// Same call again: nothing new.
	if n, _ := r.Record("7203", 5*time.Minute, bars, now); n != 0 {
		t.Fatalf("re-record wrote %d bars, want 0 (dedup by OpenTime)", n)
	}

	// 5 minutes later the forming bar completed → exactly one more row.
	if n, _ := r.Record("7203", 5*time.Minute, bars, now.Add(5*time.Minute)); n != 1 {
		t.Fatalf("wrote %d, want 1 (the newly completed bar)", n)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "7203_5m.csv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 5 { // header + 4 bars
		t.Fatalf("file has %d lines, want 5:\n%s", len(lines), raw)
	}
	if lines[0] != "DateJST;Open;High;Low;Close;Volume" {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "2026-07-21 09:00:00;") {
		t.Fatalf("first row = %q, want JST datetime prefix", lines[1])
	}
}

func TestCandleRecorder_ResumesAfterRestartWithoutDuplicates(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 21, 9, 0, 0, 0, clock.JST)
	bars := []market.Candle{
		rbar(base, time.Minute, 100),
		rbar(base.Add(time.Minute), time.Minute, 101),
	}
	now := base.Add(2 * time.Minute)

	r1 := NewCandleRecorder(dir)
	if n, _ := r1.Record("7203", time.Minute, bars, now); n != 2 {
		t.Fatalf("first writer wrote %d, want 2", n)
	}

	// Simulated bot restart: a FRESH recorder must pick up the file tail and
	// not re-append bars it already holds.
	r2 := NewCandleRecorder(dir)
	more := append(bars, rbar(base.Add(2*time.Minute), time.Minute, 102))
	if n, _ := r2.Record("7203", time.Minute, more, now.Add(time.Minute)); n != 1 {
		t.Fatalf("after restart wrote %d, want 1 (only the new bar)", n)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "7203_1m.csv"))
	if got := strings.Count(string(raw), "2026-07-21 09:00:00"); got != 1 {
		t.Fatalf("bar 09:00 appears %d times, want exactly 1", got)
	}
}

func TestCandleRecorder_EmptyAndNilAreNoops(t *testing.T) {
	r := NewCandleRecorder(t.TempDir())
	if n, err := r.Record("7203", time.Minute, nil, time.Now()); n != 0 || err != nil {
		t.Fatalf("nil bars: n=%d err=%v", n, err)
	}
}
