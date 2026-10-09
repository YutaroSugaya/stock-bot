package safety

import (
	"os"
	"path/filepath"
	"stockbot/backend/internal/testutil"
	"strings"
	"testing"
	"time"
)

func TestEmergencyStop_TripIsWriteOnceAndIdempotent(t *testing.T) {
	path := testutil.TempFlagPath(t)
	tripped := 0
	es := NewEmergencyStop(path, func(string) { tripped++ })

	if es.Active() {
		t.Fatal("should not be active before trip")
	}
	now := time.Date(2026, 6, 17, 14, 50, 0, 0, time.UTC)
	if err := es.Trip("first_reason", now); err != nil {
		t.Fatalf("trip: %v", err)
	}
	if !es.Active() {
		t.Fatal("should be active after trip")
	}
	// second trip is a no-op: original reason preserved, callback not re-fired
	if err := es.Trip("second_reason", now.Add(time.Minute)); err != nil {
		t.Fatalf("second trip: %v", err)
	}
	if tripped != 1 {
		t.Fatalf("onTrip fired %d times, want 1", tripped)
	}
	if got := es.Reason(); !strings.Contains(got, "first_reason") || strings.Contains(got, "second_reason") {
		t.Fatalf("reason not preserved (want first_reason, not second_reason): %q", got)
	}
}

func TestEmergencyStop_Resume(t *testing.T) {
	path := testutil.TempFlagPath(t)
	es := NewEmergencyStop(path, nil)
	// resume on a non-existent flag is fine
	if err := es.Resume(); err != nil {
		t.Fatalf("resume on missing flag: %v", err)
	}
	_ = es.Trip("x", time.Now())
	if err := es.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if es.Active() {
		t.Fatal("should be inactive after resume")
	}
}

func TestEmergencyStop_InMemoryFlagHaltsEvenWhenWriteFails(t *testing.T) {
	// Point the flag at an unwritable path (a file used as a directory), so the
	// file write inside Trip fails. The process must STILL halt: Active() reflects
	// the in-memory flag, and the write error is surfaced (not swallowed).
	base := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(base, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	unwritable := filepath.Join(base, "emergency_stop.flag") // base is a file → write fails

	var gotErr error
	es := NewEmergencyStop(unwritable, nil).OnTripError(func(_ string, err error) { gotErr = err })

	err := es.Trip("margin_breach", time.Now())
	if err == nil {
		t.Fatal("Trip must return the file-write error, not swallow it")
	}
	if gotErr == nil {
		t.Fatal("onTripError must fire when persistence fails")
	}
	if !es.Active() {
		t.Fatal("Active() must report tripped via the in-memory flag even when the file write failed")
	}
}

func TestEmergencyStop_Preflight(t *testing.T) {
	// A writable (creatable) directory passes and does not leave a probe behind.
	dir := filepath.Join(t.TempDir(), "runtime")
	es := NewEmergencyStop(filepath.Join(dir, "emergency_stop.flag"), nil)
	if err := es.Preflight(); err != nil {
		t.Fatalf("preflight on a creatable dir must pass: %v", err)
	}
	if es.Active() {
		t.Fatal("preflight must not trip")
	}
	if _, err := os.Stat(filepath.Join(dir, "emergency_stop.flag.preflight")); !os.IsNotExist(err) {
		t.Fatal("preflight probe file must be removed")
	}

	// An unwritable path fails closed.
	base := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(base, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := NewEmergencyStop(filepath.Join(base, "emergency_stop.flag"), nil)
	if err := bad.Preflight(); err == nil {
		t.Fatal("preflight on an unwritable path must fail closed")
	}
}

func TestPendingPositions(t *testing.T) {
	p := NewPendingPositions()
	if p.IsPending("bp-1") {
		t.Fatal("empty tracker should not report pending")
	}
	p.MarkPending("bp-1")
	if !p.IsPending("bp-1") {
		t.Fatal("bp-1 should be pending")
	}
	p.MarkResolved("bp-1")
	if p.IsPending("bp-1") {
		t.Fatal("bp-1 should be resolved")
	}
	p.MarkPending("") // empty ignored
	if p.IsPending("") {
		t.Fatal("empty id should never be pending")
	}
}
