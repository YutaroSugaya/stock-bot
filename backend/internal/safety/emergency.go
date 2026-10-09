// Package safety holds the file-based emergency stop and the pending-position
// tracker. The flag is a file (not a DB row) so it survives a bot restart; tripping
// is write-once and idempotent.
package safety

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// EmergencyStop owns the runtime emergency flag file. A trip is ALSO held in memory:
// the file is for cross-restart persistence, but the in-memory flag guarantees the
// process halts even if the file write fails — a write error must never leave the bot
// trading while it believes it tripped.
type EmergencyStop struct {
	path      string
	mu        sync.Mutex
	tripped   atomic.Bool
	onTrip    func(reason string)
	onTripErr func(reason string, err error)
}

// NewEmergencyStop builds the controller for the given flag path. onTrip may be
// nil; it fires once per Trip for counter/notify side-effects.
func NewEmergencyStop(path string, onTrip func(reason string)) *EmergencyStop {
	return &EmergencyStop{path: path, onTrip: onTrip}
}

// OnTripError registers a callback fired when a trip could not be PERSISTED to the
// flag file (the process is still halted via the in-memory flag). Wiring points it at
// an ERROR log + counter so the rare persistence failure is never swallowed.
func (e *EmergencyStop) OnTripError(fn func(reason string, err error)) *EmergencyStop {
	e.onTripErr = fn
	return e
}

// Preflight verifies at startup that the flag path is writable, so the emergency stop
// is guaranteed to engage. The caller must refuse to start when it fails, rather than
// discover it only when a trip silently no-ops. An existing flag is left untouched.
func (e *EmergencyStop) Preflight() error {
	if dir := filepath.Dir(e.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("emergency flag dir %q not creatable: %w", dir, err)
		}
	}
	if _, err := os.Stat(e.path); err == nil {
		return nil // already tripped — do not clobber
	}
	probe := e.path + ".preflight"
	if err := os.WriteFile(probe, []byte("probe\n"), 0o600); err != nil {
		return fmt.Errorf("emergency flag path %q not writable — refusing to start (the emergency stop could not engage): %w", e.path, err)
	}
	_ = os.Remove(probe)
	return nil
}

// Active reports whether the emergency stop is engaged — the in-memory flag (set the
// instant Trip is called) or the persisted flag file (a trip from a prior run).
func (e *EmergencyStop) Active() bool {
	if e.tripped.Load() {
		return true
	}
	_, err := os.Stat(e.path)
	return err == nil
}

// Trip engages the emergency stop. It sets the in-memory flag FIRST — the process
// halts immediately regardless of persistence — then writes the flag file once
// (idempotent: the first reason is preserved). A write failure is returned and
// surfaced via onTripErr, never swallowed.
func (e *EmergencyStop) Trip(reason string, now time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tripped.Store(true) // halt NOW, independent of the file write below
	if _, err := os.Stat(e.path); err == nil {
		return nil // already persisted; keep the original reason
	}
	line := fmt.Sprintf("%s %s\n", now.UTC().Format(time.RFC3339), reason)
	if err := os.WriteFile(e.path, []byte(line), 0o600); err != nil {
		werr := fmt.Errorf("write emergency flag %q: %w", e.path, err)
		if e.onTripErr != nil {
			e.onTripErr(reason, werr)
		}
		return werr
	}
	if e.onTrip != nil {
		e.onTrip(reason)
	}
	return nil
}

// Resume removes the flag file (human-only operation, exposed via
// POST /api/emergency-resume). Removing a non-existent flag is not an error.
func (e *EmergencyStop) Resume() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove emergency flag %q: %w", e.path, err)
	}
	e.tripped.Store(false)
	return nil
}

// Reason returns the trip reason line, or "" when not tripped.
func (e *EmergencyStop) Reason() string {
	b, err := os.ReadFile(e.path)
	if err != nil {
		return ""
	}
	return string(b)
}
